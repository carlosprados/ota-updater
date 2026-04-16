# OTA Update System — Edge Agent + Update Server

## Project Overview

Build a lightweight OTA (Over-The-Air) update system in Go, optimized for NB-IoT constrained networks (~20-60 kbps, high latency, frequent disconnections). The system consists of two binaries:

1. **`edge-agent`** — runs on remote devices, checks for updates, downloads deltas, applies them, and manages A/B slot swap with rollback.
2. **`update-server`** — serves update manifests and delta patches over both HTTP/REST and CoAP (RFC 7252 + Block-wise Transfer RFC 7959).

## Architecture

```
┌─────────────┐         CoAP/UDP or HTTP/TCP        ┌──────────────────┐
│  edge-agent │  ◄──────────────────────────────────► │  update-server   │
│             │   heartbeat(version_hash, device_id)  │                  │
│  ┌───┬───┐  │   ◄── manifest(delta_url, sig, meta)  │  ┌────────────┐ │
│  │ A │ B │  │   ──► chunked delta download (resume) │  │ Delta Store│ │
│  └───┴───┘  │   ──► report(success|rollback)        │  └────────────┘ │
└─────────────┘                                       └──────────────────┘
```

## Tech Stack & Dependencies

- **Language:** Go 1.22+
- **Binary diffing:** `github.com/gabstv/go-bsdiff` (bsdiff/bspatch)
- **Compression:** `github.com/klauspost/compress/zstd`
- **CoAP:** `github.com/plgd-dev/go-coap/v3`
- **Crypto:** `crypto/ed25519` (stdlib)
- **HTTP:** `net/http` (stdlib) — no frameworks
- **Storage server-side:** local filesystem (no DB required, flat file structure)
- **Config:** YAML via `gopkg.in/yaml.v3`
- **Logging:** `log/slog` (stdlib structured logging)

Use Go modules. Minimize external dependencies — prefer stdlib where possible.

## Project Structure

```
ota-update-system/
├── cmd/
│   ├── edge-agent/
│   │   └── main.go
│   └── update-server/
│       └── main.go
├── internal/
│   ├── protocol/
│   │   ├── messages.go        # Shared message types (heartbeat, manifest, report)
│   │   └── constants.go       # Protocol version, paths, defaults
│   ├── crypto/
│   │   ├── signer.go          # Ed25519 sign (server-side)
│   │   └── verifier.go        # Ed25519 verify (agent-side)
│   ├── delta/
│   │   ├── generator.go       # bsdiff delta generation (server-side)
│   │   └── patcher.go         # bspatch delta application (agent-side)
│   ├── compression/
│   │   └── zstd.go            # Compress/decompress wrappers
│   ├── agent/
│   │   ├── updater.go         # Main update orchestrator
│   │   ├── downloader.go      # Chunked download with resume (HTTP + CoAP)
│   │   ├── slots.go           # A/B slot management
│   │   ├── watchdog.go        # Post-update health check & rollback
│   │   └── config.go          # Agent configuration
│   └── server/
│       ├── http_handler.go    # HTTP transport handlers
│       ├── coap_handler.go    # CoAP transport handlers
│       ├── manifest.go        # Manifest generation & caching
│       ├── store.go           # Binary & delta storage management
│       └── config.go          # Server configuration
├── configs/
│   ├── agent.yaml
│   └── server.yaml
├── tools/
│   └── keygen/
│       └── main.go            # Ed25519 keypair generator CLI tool
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

## Detailed Specifications

### 1. Shared Protocol (`internal/protocol/`)

Define these message structures (use JSON serialization for HTTP, CBOR for CoAP):

```go
// Heartbeat — agent → server
type Heartbeat struct {
    DeviceID    string `json:"device_id"`
    VersionHash string `json:"version_hash"` // SHA-256 of current running binary
    HWInfo      HWInfo `json:"hw_info"`
    Timestamp   int64  `json:"timestamp"`
}

type HWInfo struct {
    Arch     string `json:"arch"`     // e.g. "arm64", "amd64"
    OS       string `json:"os"`       // e.g. "linux"
    FreeRAM  uint64 `json:"free_ram"` // bytes
    FreeDisk uint64 `json:"free_disk"`// bytes
}

// ManifestResponse — server → agent
type ManifestResponse struct {
    UpdateAvailable bool   `json:"update_available"`
    TargetVersion   string `json:"target_version"`
    TargetHash      string `json:"target_hash"`      // SHA-256 of target binary
    DeltaSize       int64  `json:"delta_size"`        // compressed delta size in bytes
    DeltaHash       string `json:"delta_hash"`        // SHA-256 of compressed delta
    ChunkSize       int    `json:"chunk_size"`        // recommended chunk size
    TotalChunks     int    `json:"total_chunks"`
    Signature       string `json:"signature"`         // Ed25519 sig of target binary hash (hex)
    DeltaEndpoint   string `json:"delta_endpoint"`    // relative path to download delta
}

// UpdateReport — agent → server
type UpdateReport struct {
    DeviceID       string `json:"device_id"`
    PreviousHash   string `json:"previous_hash"`
    NewHash        string `json:"new_hash"`
    Success        bool   `json:"success"`
    RollbackReason string `json:"rollback_reason,omitempty"`
    Timestamp      int64  `json:"timestamp"`
}
```

### 2. Crypto (`internal/crypto/`)

- **Key generation tool** (`tools/keygen/`): generates Ed25519 keypair, saves private key as `server.key` (PEM), public key as `agent.pub` (PEM).
- **Signer** (server): signs the SHA-256 hash of the target binary with Ed25519 private key.
- **Verifier** (agent): verifies signature using embedded/configured public key.
- Sign the **target binary hash**, not the delta. This way the agent verifies the final reconstructed binary regardless of delta path.

### 3. Delta Generation & Patching (`internal/delta/`)

- **Generator** (server-side):
  - Input: old binary path + new binary path
  - Output: bsdiff delta, then zstd-compressed (level 19)
  - Cache generated deltas on disk: `store/{from_hash}_{to_hash}.delta.zst`
  - Skip generation if cached delta already exists

- **Patcher** (agent-side):
  - Input: current binary (slot A) + compressed delta
  - Process: zstd decompress → bspatch apply → write to slot B
  - Verify SHA-256 of result matches `target_hash` from manifest
  - Verify Ed25519 signature of target hash

### 4. Edge Agent (`internal/agent/`)

#### 4.1 Configuration (`agent.yaml`)
```yaml
server:
  http_url: "http://update.example.com:8080"  # HTTP endpoint
  coap_url: "coap://update.example.com:5683"  # CoAP endpoint
  transport: "coap"  # preferred transport: "http" or "coap", falls back to the other

device:
  id: "edge-001"
  slots_dir: "/opt/agent/slots"    # directory for A/B binaries
  active_symlink: "/opt/agent/current"  # symlink pointing to active slot

update:
  check_interval: "1h"
  chunk_size: 4096          # bytes per chunk (tune for NB-IoT)
  max_retries: 10
  retry_backoff: "30s"
  watchdog_timeout: "60s"   # time to confirm health after swap

crypto:
  public_key: "/opt/agent/keys/agent.pub"

logging:
  level: "info"  # debug, info, warn, error
```

#### 4.2 A/B Slot Manager (`slots.go`)
- Maintains two slots: `slots/A` and `slots/B` (each is a binary file)
- `active_symlink` points to the currently running slot
- Methods:
  - `GetActiveSlot() (path string, hash string)` — returns active binary path and its SHA-256
  - `GetInactiveSlot() string` — returns the path of the inactive slot (write target)
  - `WriteToInactive(reader io.Reader) error` — streams new binary to inactive slot
  - `Swap() error` — atomically updates symlink to point to inactive slot (which becomes active)
  - `Rollback() error` — swaps back to previous active slot
- Use `os.Rename` for atomic symlink swap (create temp symlink, rename over existing)

#### 4.3 Chunked Downloader (`downloader.go`)
- Supports both HTTP (Range headers) and CoAP (Block2 option) transports
- **Resume capability:** tracks download progress in a small state file (`slots/.download_state`)
  ```go
  type DownloadState struct {
      DeltaHash      string `json:"delta_hash"`
      BytesReceived  int64  `json:"bytes_received"`
      TempFile       string `json:"temp_file"`
  }
  ```
- On startup, if state file exists and delta_hash matches current update, resume from `bytes_received`
- Verify each chunk via running hash (no per-chunk signatures — too expensive for NB-IoT)
- Final verification: SHA-256 of complete compressed delta must match `delta_hash` from manifest
- Exponential backoff on connection failures with jitter
- Configurable max retries

**HTTP download:**
```
GET /delta/{from_hash}/{to_hash}
Range: bytes={offset}-{offset+chunk_size-1}
```

**CoAP download:**
```
GET /delta/{from_hash}/{to_hash}
Block2 option for block-wise transfer (auto-handled by go-coap library)
```

#### 4.4 Update Orchestrator (`updater.go`)
Main update loop:
1. Compute SHA-256 of current active binary
2. Send heartbeat to server (try preferred transport, fall back to alternative)
3. If `update_available == false`, sleep until next check interval
4. Download compressed delta with resume support
5. Verify compressed delta hash
6. Decompress (zstd) and apply patch (bspatch) → write result to inactive slot
7. Verify reconstructed binary: SHA-256 must match `target_hash`
8. Verify Ed25519 signature of `target_hash`
9. Swap active symlink to new slot
10. Trigger self-restart (exec syscall to replace current process with new binary)
11. New binary runs watchdog health check
12. If watchdog passes → send success report to server
13. If watchdog fails → rollback, send failure report

#### 4.5 Watchdog (`watchdog.go`)
- After swap + restart, the new binary must confirm health within `watchdog_timeout`
- Health check: can reach server (heartbeat succeeds), basic self-diagnostics pass
- If health check fails → rollback to previous slot, restart again
- Track boot attempts in a persistent file (`slots/.boot_count`)
- If boot_count > 2 for same version hash → mark version as bad, rollback permanently, report to server
- Reset boot_count on successful health confirmation

### 5. Update Server (`internal/server/`)

#### 5.1 Configuration (`server.yaml`)
```yaml
http:
  addr: ":8080"

coap:
  addr: ":5683"

store:
  binaries_dir: "./store/binaries"   # versioned binaries: {version_hash}.bin
  deltas_dir: "./store/deltas"       # cached deltas: {from}_{to}.delta.zst

crypto:
  private_key: "./keys/server.key"

target:
  version: "1.1.0"                    # current target version label
  binary: "./store/binaries/latest"   # path to the target binary
  # target hash is computed at startup from this binary

logging:
  level: "info"
```

#### 5.2 Binary & Delta Store (`store.go`)
- On startup: compute and cache SHA-256 of target binary
- Store versioned binaries as `{hash}.bin`
- Delta generation: on-demand or pre-computed
  - When agent sends heartbeat with unknown `version_hash`, generate delta on the fly, cache it
  - For known version transitions, use cached delta
- Cleanup: optionally prune deltas for very old versions

#### 5.3 HTTP Handlers (`http_handler.go`)

Endpoints:
```
POST /heartbeat          — receive heartbeat, return manifest
GET  /delta/{from}/{to}  — serve compressed delta with Range support
POST /report             — receive update report
GET  /health             — server health check
```

- `/delta/{from}/{to}` MUST support HTTP Range requests for chunked/resumed downloads
- Content-Type for delta: `application/octet-stream`
- Add `Accept-Ranges: bytes` and `Content-Length` headers
- Return 206 Partial Content for range requests
- Return 404 if delta not available (and trigger async generation if `from` binary is known)

#### 5.4 CoAP Handlers (`coap_handler.go`)

Resources (mirroring HTTP paths):
```
POST coap://host/heartbeat
GET  coap://host/delta/{from}/{to}
POST coap://host/report
```

- Use Block-wise Transfer (Block2) for serving deltas — go-coap handles this mostly automatically
- Use CBOR encoding for structured messages (heartbeat, manifest, report) instead of JSON
- CoAP confirmable messages (CON) for reliability
- Add `github.com/fxamacker/cbor/v2` for CBOR serialization

#### 5.5 Manifest Generation (`manifest.go`)
- Given a heartbeat, determine if update is needed (compare `version_hash` with target hash)
- If update needed: check if delta exists, get its size, compute chunks count
- Sign target hash with Ed25519
- Return ManifestResponse
- If delta doesn't exist yet: return `update_available: true` but with a `retry_after` field while delta generates in background

### 6. Key Generation Tool (`tools/keygen/`)

Simple CLI:
```bash
go run ./tools/keygen -out ./keys
# Generates: keys/server.key and keys/agent.pub
```

### 7. Makefile

```makefile
.PHONY: build-agent build-server keygen test clean

LDFLAGS := -s -w

build-agent:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/edge-agent ./cmd/edge-agent

build-server:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/update-server ./cmd/update-server

keygen:
	go run ./tools/keygen -out ./keys

test:
	go test ./... -v -race

clean:
	rm -rf bin/ keys/ store/deltas/
```

### 8. Build Constraints

- **CGO_ENABLED=0** — both binaries must be fully static (no libc dependency)
- Agent binary should be as small as possible — consider build tags to exclude unused packages
- Server can be larger, no size constraint
- All code must handle context cancellation properly (graceful shutdown)
- Both binaries: handle SIGINT and SIGTERM for clean shutdown

### 9. Error Handling & Resilience

- **Agent must never panic.** Recover from all panics in goroutines.
- All I/O operations need timeouts appropriate for NB-IoT (generous: 30s+ for individual operations)
- Download failures are expected and normal — log at info level, not error
- Filesystem operations (write to slot, swap) must be as atomic as possible
- If disk is full, detect early and abort update cleanly
- Server should handle concurrent delta generation requests for the same version pair (deduplicate)

### 10. Testing Strategy

Write unit tests for:
- `internal/crypto/` — sign/verify round-trip
- `internal/delta/` — generate delta, apply patch, verify result matches original target
- `internal/compression/` — compress/decompress round-trip
- `internal/agent/slots.go` — slot management, swap, rollback
- `internal/agent/downloader.go` — mock server, test resume behavior

Write one integration test:
- Start update-server in-process
- Run agent update cycle against it (via HTTP)
- Verify agent successfully "updates" from binary A to binary B
- Place this in `integration_test.go` at project root with `//go:build integration` tag

## Implementation Order

Implement in this exact order (each step should compile and pass tests before moving on):

1. `internal/protocol/` — shared types and constants
2. `internal/crypto/` + `tools/keygen/` — key generation and sign/verify
3. `internal/compression/` — zstd wrappers
4. `internal/delta/` — delta generation and patching
5. `internal/server/store.go` — binary and delta storage
6. `internal/server/manifest.go` — manifest generation
7. `internal/server/http_handler.go` — HTTP transport
8. `internal/server/coap_handler.go` — CoAP transport
9. `internal/server/config.go` + `cmd/update-server/main.go` — server binary
10. `internal/agent/config.go` — agent configuration
11. `internal/agent/slots.go` — A/B slot management
12. `internal/agent/downloader.go` — chunked download with resume
13. `internal/agent/watchdog.go` — health check and rollback
14. `internal/agent/updater.go` — main orchestrator
15. `cmd/edge-agent/main.go` — agent binary
16. Unit tests for all packages
17. Integration test
18. Makefile, README, config examples

## Code Style

- Go idiomatic style: short variable names in small scopes, descriptive in large
- Error wrapping with `fmt.Errorf("operation: %w", err)` — always add context
- Structured logging with `slog` — include device_id, version_hash, operation in log fields
- No globals except logger. Pass dependencies explicitly.
- Interfaces at consumption site, not at definition site.
- Comments only for non-obvious decisions. No comment noise.
