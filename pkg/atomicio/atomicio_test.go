package atomicio

import (
	"bytes"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWriteFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.bin")
	data := []byte("hello atomicio")
	if err := WriteFile(path, data, 0o644, discardLogger()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestWriteFile_ReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600, discardLogger()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteFile_MissingDirFails(t *testing.T) {
	err := WriteFile("/definitely/does/not/exist/file", []byte{}, 0o644, discardLogger())
	if err == nil {
		t.Fatalf("expected error for missing parent dir")
	}
}

func TestWriteFile_CleansUpTempOnRenameFailure(t *testing.T) {
	// Force a rename failure by making the destination a directory (can't
	// overwrite a dir via rename from a regular file on Linux).
	dir := t.TempDir()
	path := filepath.Join(dir, "dest")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("x"), 0o644, discardLogger()); err == nil {
		t.Fatalf("expected error renaming over a directory")
	}
	// No .tmp-* leftovers should be in the dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file %q leaked after failure", e.Name())
		}
	}
}

func TestWriteReader_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streamed")
	payload := bytes.Repeat([]byte{0xCA, 0xFE}, 4096) // 8 KiB
	if err := WriteReader(path, bytes.NewReader(payload), 0o755, discardLogger()); err != nil {
		t.Fatalf("WriteReader: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed content differs (%d vs %d bytes)", len(got), len(payload))
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestWriteReader_PropagatesReaderError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	errReader := iotest{err: io.ErrUnexpectedEOF}
	if err := WriteReader(path, errReader, 0o644, discardLogger()); err == nil {
		t.Fatalf("expected error from bad reader")
	}
	// No leftovers.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("leftovers after reader failure: %v", entries)
	}
}

type iotest struct{ err error }

func (r iotest) Read([]byte) (int, error) { return 0, r.err }

func TestReplaceSymlink_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	targetA := filepath.Join(dir, "a")
	targetB := filepath.Join(dir, "b")
	if err := os.WriteFile(targetA, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetB, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "current")
	if err := os.Symlink(targetA, link); err != nil {
		t.Fatal(err)
	}
	// Flip to B.
	if err := ReplaceSymlink(targetB, link, discardLogger()); err != nil {
		t.Fatalf("ReplaceSymlink: %v", err)
	}
	resolved, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != targetB {
		t.Fatalf("link = %q, want %q", resolved, targetB)
	}
	// Flip again to A.
	if err := ReplaceSymlink(targetA, link, discardLogger()); err != nil {
		t.Fatal(err)
	}
	resolved, _ = os.Readlink(link)
	if resolved != targetA {
		t.Fatalf("after re-flip link = %q, want %q", resolved, targetA)
	}
}

func TestReplaceSymlink_CleansUpPriorTmp(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "current")
	// Simulate a crashed prior swap: leftover .tmp symlink.
	if err := os.Symlink("/nowhere", link+".tmp"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	_ = os.WriteFile(target, []byte("t"), 0o644)
	if err := ReplaceSymlink(target, link, discardLogger()); err != nil {
		t.Fatalf("ReplaceSymlink: %v", err)
	}
	if _, err := os.Stat(link + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("leftover .tmp not cleaned; stat err = %v", err)
	}
}

func TestConcurrent_DifferentPaths(t *testing.T) {
	// Atomicio is safe for concurrent callers over DIFFERENT paths. Verify
	// 32 goroutines writing 32 distinct files all land correctly.
	dir := t.TempDir()
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			path := filepath.Join(dir, strconv(id))
			data := []byte(strconv(id))
			if err := WriteFile(path, data, 0o644, discardLogger()); err != nil {
				t.Errorf("goroutine %d: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	entries, _ := os.ReadDir(dir)
	if len(entries) != n {
		t.Fatalf("got %d entries, want %d", len(entries), n)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// strconv formats id without pulling strconv (avoid the import churn).
func strconv(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// Compile-time check that the three helpers satisfy the shapes the callers
// use (io.Reader for WriteReader, fs.FileMode for WriteFile).
var (
	_ = WriteFile
	_ = WriteReader
	_ = ReplaceSymlink
	_ fs.FileMode
)

func TestSweepStaleTemp_OnlyOldMatches(t *testing.T) {
	dir := t.TempDir()
	// Three candidates: old tmp, fresh tmp, non-matching file.
	oldTmp := filepath.Join(dir, ".tmp-old")
	freshTmp := filepath.Join(dir, ".tmp-fresh")
	keep := filepath.Join(dir, "real-file")
	oldPartial := filepath.Join(dir, "download.partial")
	for _, p := range []string{oldTmp, freshTmp, keep, oldPartial} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate the "old" files 48h.
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{oldTmp, oldPartial} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	removed, failed := SweepStaleTemp(
		dir,
		[]string{".tmp-", "download."},
		24*time.Hour,
		discardLogger(),
	)
	if removed != 2 || failed != 0 {
		t.Fatalf("SweepStaleTemp removed=%d failed=%d, want 2/0", removed, failed)
	}
	// Fresh tmp must survive (prefix match but age below threshold).
	if _, err := os.Stat(freshTmp); err != nil {
		t.Fatalf("fresh tmp should survive: %v", err)
	}
	// Non-matching file must survive regardless of age.
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-matching file should survive: %v", err)
	}
	// Old matching files removed.
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Fatalf("old tmp should have been removed; stat err = %v", err)
	}
	if _, err := os.Stat(oldPartial); !os.IsNotExist(err) {
		t.Fatalf("old .partial should have been removed; stat err = %v", err)
	}
}

func TestSweepStaleTemp_MissingDirIsSilent(t *testing.T) {
	removed, failed := SweepStaleTemp(
		"/nonexistent/path",
		[]string{".tmp-"},
		time.Hour,
		discardLogger(),
	)
	if removed != 0 || failed != 0 {
		t.Fatalf("missing dir should be a quiet no-op, got removed=%d failed=%d", removed, failed)
	}
}
