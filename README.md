# OTA Updater

Lightweight OTA (Over-The-Air) update system in Go, tuned for **NB-IoT
constrained networks** (~20-60 kbps, high latency, frequent disconnects).
Ships two binaries plus a keygen tool.

> **Status — feature-complete.** All 18 steps of the implementation plan
> are done; both binaries build static, the full unit-test suite is green,
> and an end-to-end integration test passes. See `CLAUDE.md` for the
> per-step changelog.

---

## Components

| What | Where | Role |
|---|---|---|
| `update-server` | `cmd/update-server/` | Serves signed manifests and compressed delta patches over HTTP and CoAP. fsnotify auto-reload; admin control plane (`/admin/reload`, `/admin/loglevel`) behind a Bearer token. |
| `edge-agent` | `cmd/edge-agent/` *(pending, step 15)* | Runs on the device. Heartbeats, downloads deltas, applies patches, manages A/B slots and rollback. Will also be available as a Go library (`pkg/agent/`) so any Go executable can embed update logic. |
| `keygen` | `tools/keygen/` | Generates the Ed25519 keypair once. |

## Prerequisites

- Go 1.22 or newer (project developed on 1.25).
- [Task](https://taskfile.dev) runner:
  ```sh
  go install github.com/go-task/task/v3/cmd/task@latest
  ```

## Build

```sh
task build            # both binaries (static, CGO_ENABLED=0)
task build-server     # just the server
task build-agent      # just the agent
task keygen           # Ed25519 keypair in ./keys
task test             # full suite with -race
task --list           # see everything available
```

Binaries land in `./bin/`. Keys in `./keys/`.

---

## Release workflow (operator perspective)

The whole point of this system is to take **manual signing and server
restarts out of the release loop**.

### Once, at first setup

```sh
task keygen
# writes:
#   keys/server.key   (0600, PKCS#8 PEM, Ed25519 private)  — stays on the server
#   keys/agent.pub    (0644, PKIX PEM,  Ed25519 public)    — ship with the agent/firmware
```

### On every release

```sh
# 1. Build your new target binary (any Go executable).
go build -o my-new-binary ./...

# 2. Drop it into the target location configured in server.yaml
#    (defaults to ./store/binaries/latest).
cp my-new-binary ./store/binaries/latest
```

That's it. The server picks up the change **without restart**:

- An `fsnotify` watcher on the target path auto-reloads within a few
  hundred milliseconds.
- Alternatively, your CI/CD can call the admin endpoint explicitly
  (see below).

The server then computes the new `target_hash`, regenerates deltas
on-demand as agents check in, and signs each manifest on the fly with
the in-memory private key. **No per-release signing command to run.**

### Running the server

```sh
task build-server
./bin/update-server -config ./configs/server.yaml
```

An example config ships at `configs/server.yaml`. Minimum required
fields: `crypto.private_key`, `target.binary`, `admin.token`. The rest
have sensible defaults (HTTP on `:8080`, CoAP on `:5683`, text logs at
INFO). Send `SIGINT` or `SIGTERM` for a graceful shutdown that honors
`http.shutdown_timeout` (default `15s`).

---

## Admin control plane

Both admin endpoints share one authentication scheme: **static Bearer
token**, configured in `server.yaml` under `admin.token`. Requests must
carry `Authorization: Bearer <token>`; the server compares in constant
time. Mismatch or missing header → `401 Unauthorized`.

The endpoints are intended for **internal networks or loopback**. Expose
them only where you control who reaches them (reverse proxy + ACL, VPN,
loopback-only bind).

### `POST /admin/reload`

Explicit reload trigger — useful in CI/CD pipelines that want synchronous
confirmation rather than waiting for the filesystem watcher.

```sh
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://update.example.com:8080/admin/reload
```

Responses:

- `200 OK` → `{"target_hash": "<new-sha>", "previous_hash": "<old-sha>"}`.
- `401 Unauthorized` on auth failure.
- `500 Internal Server Error` if the reload itself failed (e.g. target
  file missing). The previous target remains active — the server never
  ends up in a broken state.

The reload also invalidates the in-memory signed-manifest cache so the
next heartbeat produces a manifest signed over the new `target_hash`.

### `POST /admin/loglevel`

Change the log level at runtime without restarting or reopening the log
sink. Accepts JSON `{"level": "<level>"}` where level is one of
`debug|info|warn|error`.

```sh
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"level":"debug"}' \
  http://update.example.com:8080/admin/loglevel
```

Responses:

- `200 OK` → `{"level":"debug"}` when applied.
- `400 Bad Request` for unknown levels or malformed JSON.
- `401 Unauthorized` on auth failure.

### Operational notes

- Old cached deltas (`{from}_{oldTarget}.delta.zst`) remain on disk
  after a reload but are never served: the new `target_hash` makes them
  unreachable. Prune manually or via future tooling.
- The server caps body size (4 KiB) on heartbeat/report, panic-recovers
  every handler into a structured 500/5.00 response, and bounds
  concurrent bsdiff runs to protect CPU/RAM under bursty load from many
  distinct source versions.

---

## Security & signature scheme

The cryptographic details are **non-trivial** and live in their own
authoritative document:

- **[`docs/signing.md`](docs/signing.md)** — canonical payload, keys,
  server flow, **agent verification order**, release workflow, threat
  model, and why option B (`sign(targetHash || deltaHash)`) was chosen
  over the alternatives.

Any change to the signing code must update that document in the same
commit.

### TL;DR

- Ed25519 signatures, produced online by the server from
  `keys/server.key`.
- The agent verifies against the bundled `keys/agent.pub` **before**
  downloading the delta (rejects bad manifests with zero bytes
  transferred) and again **after** download but **before** applying
  `bspatch` (rejects corrupt deltas without wasting CPU).
- You never sign anything by hand.

---

## Embedding as a library

The agent lives in `pkg/agent/` and is designed for external import. Any
Go binary can embed the same OTA loop that powers `cmd/edge-agent` —
heartbeats, signed-manifest verification, delta download, A/B swap,
watchdog and self-restart — without forking the project.

`cmd/edge-agent/main.go` is a thin (~190 line) reference implementation.
The minimum viable embedder looks like this:

```go
package main

import (
    "context"
    "log"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "path/filepath"
    "syscall"
    "time"

    "github.com/amplia/ota-updater/pkg/agent"
    "github.com/amplia/ota-updater/pkg/crypto"
    "github.com/amplia/ota-updater/pkg/protocol"
)

func main() {
    logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

    pubKey, err := crypto.LoadPublicKey("/etc/myapp/agent.pub")
    if err != nil {
        log.Fatal(err)
    }

    slotsDir := "/var/lib/myapp/slots"
    slots, err := agent.NewSlotManager(slotsDir, "/var/lib/myapp/current", logger)
    if err != nil {
        log.Fatal(err)
    }
    bootCounter, err := agent.NewBootCounter(filepath.Join(slotsDir, agent.BootCountFileName))
    if err != nil {
        log.Fatal(err)
    }

    // Pick one transport. NewClientPair enforces that the heartbeat client
    // and the delta transport speak the same wire protocol.
    httpClient := &http.Client{}
    primary, err := agent.NewClientPair(
        agent.NewHTTPClient("http://updates.example.com:8080", httpClient),
        agent.NewHTTPTransport(httpClient),
    )
    if err != nil {
        log.Fatal(err)
    }

    // The watchdog probe re-uses the primary client to confirm the new
    // binary can reach the server after a swap.
    checker := &agent.DefaultHealthChecker{
        Heartbeat: func(ctx context.Context) error {
            _, hash, _, err := slots.ActiveSlot()
            if err != nil {
                return err
            }
            _, err = primary.Client.Heartbeat(ctx, &protocol.Heartbeat{
                DeviceID:    "device-001",
                VersionHash: hash,
                Timestamp:   time.Now().Unix(),
            })
            return err
        },
    }
    watchdog, err := agent.NewWatchdog(bootCounter, checker, agent.WatchdogConfig{}, logger)
    if err != nil {
        log.Fatal(err)
    }

    updater, err := agent.NewUpdater(agent.UpdaterDeps{
        Config: agent.UpdaterConfig{
            DeviceID: "device-001",
            StateDir: slotsDir,
        },
        Primary:   primary,
        Slots:     slots,
        PublicKey: pubKey,
        Watchdog:  watchdog,
        Restart:   agent.ExecRestart{}, // or agent.ExitRestart{} under a supervisor
        Logger:    logger,
    })
    if err != nil {
        log.Fatal(err)
    }

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := updater.Run(ctx); err != nil && err != context.Canceled {
        log.Fatal(err)
    }
}
```

Customization points the API exposes by design:

- **`RestartStrategy`** — `ExecRestart` (default, `syscall.Exec`) or
  `ExitRestart` for supervisor-driven restarts. Implement the interface
  for anything else.
- **`HealthChecker`** — the post-swap probe. The default heartbeats the
  server; richer embedders can compose application-level checks.
- **`HWInfoFunc`** in `UpdaterDeps` — report free RAM/disk or hardware
  identifiers the server can use for fleet analytics.
- **`Logging.SetLevel`** — change the slog level at runtime without
  restarting the process.
- **Library-only flow** — `Updater.RunOnce(ctx)` and `Updater.BootPhase(ctx)`
  are exported so embedders can drive the lifecycle from their own loop
  instead of `Updater.Run`.

The companion packages — `pkg/protocol`, `pkg/crypto`, `pkg/delta`,
`pkg/compression` — are also exported. Re-using them from another
binary is supported.

---

## Update policy (semver + auto-update)

The agent decides whether to apply an available update based on its own
**baked-in semver** versus `ManifestResponse.TargetVersion`, gated by two
independent knobs in `configs/agent.yaml`:

```yaml
update:
  auto_update: true                 # master switch
  max_bump: "major"                 # none | patch | minor | major
  unknown_version_policy: "deny"    # deny | allow when TargetVersion is not semver
```

- **`auto_update: false`** — the agent detects and logs updates but never
  applies them automatically. Good for controlled fleets where rollouts
  are triggered by an external control plane.
- **`max_bump`** caps the size of the semver transition accepted
  automatically when `auto_update: true`. An update whose bump exceeds
  this cap is logged but not applied:
  - `none` — block all automatic updates (equivalent to `auto_update: false`).
  - `patch` — only accept `1.2.3 → 1.2.4`.
  - `minor` — accept patch + minor (e.g. `1.2.3 → 1.3.0`).
  - `major` — accept everything (default).
- **`unknown_version_policy`** — what to do when the server's
  `TargetVersion` is not valid semver (legacy labels, typos, tampering):
  - `deny` (default) — refuse to apply. Conservative.
  - `allow` — apply anyway, preserving the pre-semver behavior.

### Injecting the agent's version at build time

The agent learns its own version from an `ldflags`-injected string. The
`Taskfile.yml` does this automatically from `git describe`:

```bash
task build-agent                    # bin/edge-agent reports `1.2.3` (or `abc1234-dirty`)
bin/edge-agent -version             # prints the value
```

Manually (or from library embedders):

```bash
go build -ldflags="-X main.version=1.2.3" ./cmd/edge-agent
```

An empty version at runtime **disables** the semver gate entirely — the
agent still reports every update available from the server. Operators who
want strict gating always inject a version.

### Manual triggers (bypass the policy for one cycle)

When `auto_update: false` or when an update is blocked by `max_bump`, the
operator can force a **single** cycle to apply anyway. Two equivalent
mechanisms:

**1. Sidecar file `<slots_dir>/.update_now`** — ops-friendly, accessible via
SSH / ansible / systemd drop-in without agent code changes:

```bash
# On the device:
touch /opt/agent/slots/.update_now
```

The next `RunOnce` detects the file, consumes it (removes it), and runs
the cycle ignoring `auto_update` and `max_bump`. The file is consumed
whether or not the cycle actually applies an update — this prevents a
stuck trigger from repeatedly bypassing the policy on later cycles.

**2. Library API `Updater.TriggerUpdate()`** — for embedders that own
their own control plane:

```go
updater, _ := agent.NewUpdater(deps)

// ... later, when your control plane says "go":
updater.TriggerUpdate()

// Next RunOnce cycle will ignore auto_update / max_bump exactly once.
```

Both triggers are one-shot and race-safe. They can be combined (an
embedder may both touch the sidecar AND call `TriggerUpdate()`); the cycle
still only skips the gate once.

### Heartbeat includes version

Each heartbeat now carries a `version` field (optional, free-form string
that should be semver when the agent knows its own). The server logs it
alongside `version_hash` so operators can query the fleet by human label
instead of SHA only:

```json
{
  "device_id": "edge-001",
  "version_hash": "d2a3...",
  "version": "1.2.3",
  "hw_info": { "arch": "arm64", "os": "linux" },
  "timestamp": 1713300000
}
```

`version_hash` remains the authoritative identity on the wire; `version`
is advisory.

---

## Self-restart after swap (service manager compatibility)

After a successful A/B swap the agent must re-exec so the freshly
activated binary takes over. The design is:

- **Default strategy: `syscall.Exec`.** Replaces the running process
  image with the new binary while keeping the **same PID, cgroup,
  environment variables and inherited file descriptors**. No downtime
  and no dependency on any particular service manager.
- **Pluggable via the `RestartStrategy` interface.** Library consumers
  that prefer a clean exit + supervisor-driven restart can inject
  `ExitRestart` (ships in-box) or their own implementation.

### systemd

Fully compatible. systemd tracks services by PID and cgroup, both of
which survive `syscall.Exec`.

| `Type=` | Behavior after self-exec |
|---|---|
| `simple` / `exec` (most common) | Transparent. PID unchanged → service stays active. No `Restart=` cycle consumed. |
| `notify` | The new binary must re-send `sd_notify(READY=1)`. `NOTIFY_SOCKET` survives the exec, so the helper used at first start works again. |
| `forking` | Not relevant (we exec, we don't fork). |

Additional notes:

- `WatchdogSec=` (systemd hardware/software watchdog) — the
  `NOTIFY_SOCKET` is inherited, so `WATCHDOG=1` pings resume from the
  new binary without missing a beat.
- `Restart=on-failure` + `StartLimitBurst=` — self-restart **does not
  consume** the restart budget because no process exit happens. Only
  real crashes count. This is usually what you want: you can distinguish
  a buggy binary (crashes → systemd restart counter trips) from a clean
  OTA handover.
- `ExecStart=` should point at a **stable path** (typically the A/B
  symlink, e.g. `/var/lib/edge-agent/slots/current/edge-agent`). On
  first start systemd launches through the symlink; after a swap
  `syscall.Exec` re-execs the binary the symlink now resolves to. If
  the service ever crashes and systemd restarts it, it naturally picks
  up the most recently activated slot.

Minimal unit reference:

```ini
[Unit]
Description=Edge Agent
After=network-online.target

[Service]
Type=simple
ExecStart=/var/lib/edge-agent/slots/current/edge-agent -config /etc/edge-agent.yaml
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

### Docker

Fully compatible. `syscall.Exec` replaces the process image while
keeping the PID, so a container whose entrypoint is the agent **does
not die during self-update** — Docker keeps seeing PID 1 alive.

Caveats specific to container deployments:

- **Persist the A/B slots on a volume.** Writes to the container's
  writable layer are lost on container restart. Mount `slots_dir` on
  a named volume or bind mount so a rebuilt container finds the
  previously activated slot:

  ```yaml
  services:
    edge-agent:
      image: your/edge-agent:bootstrap
      volumes:
        - agent-slots:/var/lib/edge-agent/slots
        - agent-state:/var/lib/edge-agent/state
      restart: unless-stopped
  volumes:
    agent-slots:
    agent-state:
  ```

- **Signal handling at PID 1.** If the agent is PID 1 inside the
  container it must handle `SIGTERM`/`SIGINT`; the agent does. If you
  prefer a supervisor, run `tini`/`dumb-init` as PID 1 — the agent
  becomes a child and `syscall.Exec` still works (the child's image is
  replaced, tini keeps reaping).
- **Bootstrap image is just a launcher.** The image only needs to
  contain an initial agent binary. All future versions arrive via OTA
  and land in the mounted slots volume. Image rebuilds are reserved for
  infrastructure changes (base image, CA certs, etc.), not for
  application updates.
- **Restart policy.** Use `restart: unless-stopped` (or
  `restart: always`) so a real crash is still caught, while normal
  self-updates remain invisible to the Docker supervisor.

### When to use `ExitRestart` instead

Prefer the alternative `ExitRestart` strategy when:

- The host supervisor enforces a "fresh process per start" invariant
  (custom init, specific audit/logging pipeline, etc.).
- You need systemd's `StartLimitBurst=` to count OTA-driven restarts
  (unusual, but valid if you want a hard ceiling on update churn).
- You are embedding the agent as a library and your own binary already
  owns the self-restart path.

In all other cases `syscall.Exec` is the right default.

---

## Memory bounds (server, 24/7 operation)

The update-server is designed for **strictly bounded RAM** regardless of
the size of its history: 100 binaries or 10 000, the memory footprint is
governed by three knobs in `configs/server.yaml` and nothing else.

```yaml
store:
  target_max_memory_mb: 200   # active target binary: kept in RAM iff it fits
  hot_delta_cache_mb: 512     # hot LRU of compressed delta bytes
  delta_concurrency: 2        # max concurrent bsdiff runs

manifest:
  cache_size: 4096            # signed-manifest LRU entry count
```

### What is held in RAM

| Item | Bound | Notes |
|---|---|---|
| Active target binary | ≤ `target_max_memory_mb` | Above the cap, re-read from disk per generation. |
| Source binaries (historical) | **0** | Never held. Read from disk at each bsdiff generation; the kernel page cache handles OS-level LRU. |
| Hot delta cache | ≤ `hot_delta_cache_mb` | Byte-budget LRU. Values bigger than the whole budget are rejected (never cached). |
| Signed manifests | ≤ `cache_size` entries × ~500 B | Entry-count LRU. |
| Per-generation bsdiff transient | ~20× the larger of (source, target) | Bounded by `delta_concurrency`. |

### Request path `/delta/{from}/{to}`

```
1. hotDeltas.Get(key)
   ├─ hit  → serve bytes from RAM, no I/O
   └─ miss → ↓

2. disk exists?
   ├─ yes → read file ONCE via singleflight (collapses concurrent campaign
   │         bursts into one os.Open), populate hotDeltas, serve bytes
   └─ no  → dispatch async bsdiff generation, reply 404+RetryAfter

3. generateAndCache (one at a time per key, bounded by delta_concurrency):
   - load oldBin from disk; load targetBin from RAM or disk
   - bsdiff + zstd → bytes
   - writeAtomic to disk + hotDeltas.Put
```

During a campaign where thousands of agents ask for the same delta,
`singleflight` on the post-miss disk read means **one file open and one
allocation** instead of thousands. This is what keeps the server stable
under coordinated rollouts.

### Sizing the knobs

- **`target_max_memory_mb`** — set to the biggest target you plan to ship
  plus some headroom. Past the cap the server still works, but every
  delta generation re-reads the target from disk (I/O cost, not a
  correctness issue).
- **`hot_delta_cache_mb`** — size for one typical campaign. A rollout
  that fans a single 10 MiB delta to thousands of devices fits
  comfortably at the default. Bursts that span many different deltas
  benefit from a higher cap; evictions are graceful (LRU drops the
  coldest).
- **`delta_concurrency`** — each concurrent bsdiff peaks at roughly 20×
  the binary size in RAM. With a 50 MiB target and `delta_concurrency=2`,
  reserve ~2 GiB of RAM for generation on top of the caps above.

### Known limits

- **bsdiff is the delta algorithm**. For targets much bigger than ~100 MiB
  its RAM footprint (~20× the binary) gets uncomfortable. We evaluated
  librsync and rejected it: its deltas are **~100× larger than bsdiff's**
  on realistic Go binaries (a single string change produces a 705 KiB
  rsync delta vs 5.8 KiB bsdiff). On NB-IoT downlink, that multiplier
  dominates over any RAM savings. See `benchmark/` for the harness.

---

## Implementation progress

The 18-step plan from `prompt-ota-updater.md` is complete. The detailed
checklist with per-step notes lives in `CLAUDE.md`.

---

## Repo layout

```
cmd/
  edge-agent/        # thin wrapper around pkg/agent.Updater
  update-server/     # HTTP+CoAP server entrypoint
pkg/                 # exported, importable as a library
  agent/             # device-side orchestrator: Updater, SlotManager, Watchdog,
                     #   ProtocolClient (HTTP+CoAP), Downloader, RestartStrategy
  protocol/          # wire types (JSON + CBOR) shared by HTTP and CoAP
  crypto/            # Ed25519 sign/verify + PEM key I/O
  delta/             # bsdiff + zstd patch pipeline
  compression/       # zstd wrappers
internal/
  server/            # binary-only: only consumed by cmd/update-server
integration/         # //go:build integration end-to-end test
tools/keygen/        # Ed25519 keypair generator CLI
docs/
  signing.md         # authoritative signature scheme reference
configs/             # example agent.yaml and server.yaml
Taskfile.yml         # build/test/ci task runner config
```
