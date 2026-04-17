package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"time"

	"github.com/amplia/ota-updater/pkg/atomicio"
)

// ErrResumeUnsupported is returned by transports that can't honor a non-zero
// start offset. The Downloader catches it and restarts the transfer from 0.
var ErrResumeUnsupported = errors.New("transport does not support resume")

// ErrHashMismatch is returned when the full downloaded payload's SHA-256
// does not match the expected value from the manifest.
var ErrHashMismatch = errors.New("downloaded delta hash does not match manifest")

// DeltaTransport abstracts the wire protocol the Downloader uses to fetch
// bytes. Implementations must honor context cancellation. (Named to avoid
// collision with the Transport string type used by Config.)
type DeltaTransport interface {
	// Name returns a short identifier ("http", "coap") used for state/logs.
	Name() string
	// FetchRange returns a reader for the payload starting at effectiveOffset.
	// If the transport or server ignored the requested offset, effectiveOffset
	// is 0 and the caller must truncate any partial state. A zero effective
	// offset with a non-zero requested offset is not an error — it's a signal
	// to restart.
	FetchRange(ctx context.Context, rawURL string, offset int64) (body io.ReadCloser, effectiveOffset int64, err error)
}

// FetchTarget describes one delta download.
type FetchTarget struct {
	URL       string // full URL with scheme (http://, https://, coap://)
	DeltaHash string // expected SHA-256 hex of the compressed delta
	TotalSize int64  // from manifest, in bytes (0 = unknown)
	OutPath   string // final on-disk path after successful verification
}

// DownloaderConfig tunes retry and state behavior.
type DownloaderConfig struct {
	StatePath    string        // JSON state file for resume; ignored when empty
	MaxRetries   int           // total attempts = MaxRetries + 1 (first try + retries)
	RetryBackoff time.Duration // initial backoff; doubles per failure, capped at 5m
}

// Downloader orchestrates one transport and persists resume state so an
// NB-IoT device that crashes mid-download picks up where it left off.
type Downloader struct {
	transport DeltaTransport
	cfg       DownloaderConfig
	logger    *slog.Logger
	rand      *rand.Rand
}

// NewDownloader wires a Downloader with the given transport and config.
func NewDownloader(transport DeltaTransport, cfg DownloaderConfig, logger *slog.Logger) *Downloader {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 30 * time.Second
	}
	return &Downloader{
		transport: transport,
		cfg:       cfg,
		logger:    logger,
		rand:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// downloadState is the JSON shape of the resume state file.
type downloadState struct {
	DeltaHash     string `json:"delta_hash"`
	BytesReceived int64  `json:"bytes_received"`
	TempFile      string `json:"temp_file"`
	Transport     string `json:"transport"`
}

// Download runs the retry loop until the target is fully fetched and
// verified, or ctx is cancelled, or MaxRetries is exhausted. Returns
// ErrHashMismatch if the payload's hash does not match the manifest.
func (d *Downloader) Download(ctx context.Context, tgt FetchTarget) error {
	st := d.loadState(tgt)
	tmpPath := tgt.OutPath + ".partial"
	offset := int64(0)
	if st != nil {
		tmpPath = st.TempFile
		offset = st.BytesReceived
		d.logger.Info("resuming download",
			"op", "download", "offset", offset, "delta_hash", tgt.DeltaHash, "transport", d.transport.Name(),
		)
	}

	backoff := d.cfg.RetryBackoff
	var lastErr error
	success := false
	for attempt := 0; attempt <= d.cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		newOffset, err := d.attempt(ctx, tgt, tmpPath, offset)
		if err == nil {
			offset = newOffset
			success = true
			break
		}
		lastErr = err
		offset = newOffset

		if errors.Is(err, ErrResumeUnsupported) {
			d.logger.Info("transport rejected resume, restarting",
				"op", "download", "transport", d.transport.Name(),
			)
			_ = os.Remove(tmpPath)
			offset = 0
		}

		d.logger.Warn("download attempt failed",
			"op", "download", "attempt", attempt+1, "transport", d.transport.Name(),
			"offset", offset, "err", err,
		)
		d.saveState(tgt, tmpPath, offset)

		if attempt == d.cfg.MaxRetries {
			break
		}

		wait := jitter(backoff, d.rand)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
	}

	if !success {
		return fmt.Errorf("download failed after %d attempts: %w", d.cfg.MaxRetries+1, lastErr)
	}

	actual, err := hashFile(tmpPath)
	if err != nil {
		return fmt.Errorf("hash downloaded delta: %w", err)
	}
	if actual != tgt.DeltaHash {
		_ = os.Remove(tmpPath)
		d.clearState()
		return fmt.Errorf("%w: got %s", ErrHashMismatch, actual)
	}
	if err := os.Rename(tmpPath, tgt.OutPath); err != nil {
		return fmt.Errorf("finalize delta: %w", err)
	}
	d.clearState()
	d.logger.Info("download complete",
		"op", "download", "path", tgt.OutPath, "size", offset, "transport", d.transport.Name(),
	)
	return nil
}

// attempt runs one FetchRange + copy cycle, returning the updated offset.
// On ErrResumeUnsupported the partial file must be discarded by the caller.
func (d *Downloader) attempt(ctx context.Context, tgt FetchTarget, tmpPath string, offset int64) (int64, error) {
	body, effOffset, err := d.transport.FetchRange(ctx, tgt.URL, offset)
	if err != nil {
		return offset, err
	}
	defer body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	f, err := os.OpenFile(tmpPath, flags, 0o644)
	if err != nil {
		return offset, fmt.Errorf("open tmp file: %w", err)
	}
	defer f.Close()

	if effOffset != offset {
		// Server ignored Range (e.g. returned 200 OK instead of 206). Truncate
		// and write from the start.
		d.logger.Warn("server ignored range, restarting transfer",
			"op", "download", "requested", offset, "effective", effOffset,
		)
		if err := f.Truncate(0); err != nil {
			return offset, fmt.Errorf("truncate tmp: %w", err)
		}
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, fmt.Errorf("seek tmp: %w", err)
	}
	n, err := io.Copy(f, body)
	offset += n
	if err != nil {
		return offset, err
	}
	return offset, nil
}

func (d *Downloader) loadState(tgt FetchTarget) *downloadState {
	if d.cfg.StatePath == "" {
		return nil
	}
	data, err := os.ReadFile(d.cfg.StatePath)
	if err != nil {
		return nil
	}
	var st downloadState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	if st.DeltaHash != tgt.DeltaHash || st.Transport != d.transport.Name() {
		// Target or transport changed; discard stale state.
		return nil
	}
	return &st
}

func (d *Downloader) saveState(tgt FetchTarget, tmpPath string, offset int64) {
	if d.cfg.StatePath == "" {
		return
	}
	st := downloadState{
		DeltaHash:     tgt.DeltaHash,
		BytesReceived: offset,
		TempFile:      tmpPath,
		Transport:     d.transport.Name(),
	}
	data, err := json.Marshal(st)
	if err != nil {
		d.logger.Warn("save state marshal", "err", err)
		return
	}
	// Atomic + durable: a crash mid-write used to corrupt the JSON, which
	// loadState silently tolerated by discarding the whole resume state —
	// losing minutes of NB-IoT downlink. With atomicio.WriteFile the worst
	// case is "state unchanged", never "state corrupt".
	if err := atomicio.WriteFile(d.cfg.StatePath, data, 0o644, d.logger); err != nil {
		d.logger.Warn("save state write", "err", err)
	}
}

func (d *Downloader) clearState() {
	if d.cfg.StatePath == "" {
		return
	}
	_ = os.Remove(d.cfg.StatePath)
}

// jitter returns d ± 30% random variation to avoid retry thundering herds.
func jitter(d time.Duration, r *rand.Rand) time.Duration {
	delta := float64(d) * 0.3
	offset := (r.Float64()*2 - 1) * delta
	result := time.Duration(float64(d) + offset)
	if result < 0 {
		return d
	}
	return result
}
