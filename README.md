# OTA Updater

Lightweight OTA (Over-The-Air) update system in Go, tuned for **NB-IoT
constrained networks** (~20-60 kbps, high latency, frequent disconnects).
Ships two binaries plus a keygen tool.

> **Status — feature-complete.** All 18 steps of the implementation plan
> are done; both binaries build static, the full unit-test suite is green,
> and an end-to-end integration test passes. See `CLAUDE.md` for the
> per-step changelog.

---

## Install

Every package under `pkg/` is importable:

```sh
go get github.com/carlosprados/ota-updater@latest
```

```go
import (
    "github.com/carlosprados/ota-updater/pkg/agent"
    "github.com/carlosprados/ota-updater/pkg/delta"
    "github.com/carlosprados/ota-updater/pkg/protocol"
)
```

Licensed under [Apache 2.0](LICENSE). See
[Embedding as a library](#embedding-as-a-library) for a self-contained example.

## Documentation

**<https://carlosprados.github.io/ota-updater/>** — the full documentation
site, with architecture, sequence, state and flow diagrams covering how the
system works and what it buys you. Republished automatically on every version
tag.

This README stays the reference for operators running the binaries; the site
is the better starting point if you are evaluating the library or need to see
the moving parts.

## Components

| What | Where | Role |
|---|---|---|
| `update-server` | `cmd/update-server/` | Serves signed manifests, compressed delta patches and whole-binary fallbacks over HTTP and CoAP, for any number of artifacts. fsnotify auto-reload; admin control plane behind a Bearer token; optional retention sweeper. Importable as `pkg/server`. |
| `edge-agent` | `cmd/edge-agent/` | Runs on the device. Heartbeats, downloads deltas (or whole binaries), applies patches, manages A/B slots and rollback. Also available as a Go library (`pkg/agent/`) so any Go executable can embed update logic. |
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

# 2. Drop it into the artifact's `binary` path from server.yaml.
cp my-new-binary ./store/binaries/latest
```

That's it. The server picks up the change **without restart**:

- An `fsnotify` watcher on each artifact's `binary` path auto-reloads
  within a few hundred milliseconds.
- Alternatively, your CI/CD can call the admin endpoint explicitly:
  `POST /admin/reload` for everything, or
  `POST /admin/reload {"artifact":"agent/linux/arm64"}` for one track —
  so deploying one component never disturbs the others.

The server then computes the new `target_hash` for that artifact,
regenerates deltas on-demand as agents check in, and signs each manifest
on the fly with the in-memory private key. **No per-release signing
command to run.**

### Running the server

```sh
task build-server
./bin/update-server -config ./configs/server.yaml
```

An example config ships at `configs/server.yaml`. Minimum required
fields: `crypto.private_key`, `admin.token`, and at least one entry under
`artifacts:` (or a legacy `target.binary`). The rest have sensible
defaults (HTTP on `:8080`, CoAP on `:5683`, text logs at INFO). Send
`SIGINT` or `SIGTERM` for a graceful shutdown that honors
`http.shutdown_timeout` (default `15s`).

---

## Admin control plane

Both admin endpoints share one authentication scheme: **static Bearer
token**, configured in `server.yaml` under `admin.token`. Requests must
carry `Authorization: Bearer <token>`; the server compares in constant
time. Mismatch or missing header → `401 Unauthorized`.

**Token requirements**: the server rejects any `admin.token` shorter
than 32 characters at config load, roughly 128 bits of entropy when
the token is random hex or base64. Generate one with:

```sh
openssl rand -hex 16        # 32 hex chars
openssl rand -base64 24     # 32 base64 chars
```

The endpoints are intended for **internal networks or loopback**. Expose
them only where you control who reaches them (reverse proxy + ACL, VPN,
loopback-only bind).

**Rate limit on auth failures**: a token-bucket throttles only 401
responses (missing or wrong token). Legitimate requests with the correct
token never touch the limiter — CI/CD pipelines that hit `/admin/reload`
repeatedly are unaffected. Defaults in `server.yaml`:

```yaml
admin:
  rate_limit_per_sec: 5      # refill rate for the failure bucket; 0 disables
  rate_limit_burst: 20       # bucket size
```

When the bucket is exhausted, the server responds `429 Too Many Requests`
with `Retry-After: 1` instead of the usual 401. A 32-char random token
combined with 5/s of failures is infeasible to brute-force; the limiter
is a belt-and-suspenders layer on top of network access control.

**Current scope**: the rate limit is **global**, not per-IP. A
distributed attack from many source IPs saturates the same bucket.
Operational mitigation: firewall the admin port to known sources (your
CI/CD host, your jump box). Per-IP rate limiting is a tracked
follow-up — see "Known limitations" below.

### `POST /admin/reload`

Explicit reload trigger — useful in CI/CD pipelines that want synchronous
confirmation rather than waiting for the filesystem watcher.

```sh
# Reload every file-backed artifact.
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://update.example.com:8080/admin/reload

# Reload just one track — a per-component pipeline should always do this,
# so deploying one service never republishes the others.
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"artifact":"keystone-agent/linux/arm64"}' \
  http://update.example.com:8080/admin/reload
```

Responses:

- `200 OK` → `{"reloaded": {"<artifact>": "<new-target-hash>", …}}`.
- `400 Bad Request` for a malformed artifact key.
- `401 Unauthorized` on auth failure.
- `404 Not Found` for an unknown artifact, or when no artifact is
  file-backed.
- `500 Internal Server Error` if the reload itself failed (e.g. source
  file missing). The previous target remains active — the server never
  ends up in a broken state.

A reload that changes a target also invalidates the in-memory
signed-manifest cache, so the next heartbeat produces a manifest signed
over the new `target_hash`.

### `GET|POST|DELETE /admin/artifacts`

Manage publication tracks without editing YAML or restarting.

```sh
# List every track and which one answers unnamed heartbeats.
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://update.example.com:8080/admin/artifacts

# Publish (or update) a track from a path on the server's filesystem.
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"keystone-proxy","os":"linux","arch":"amd64",
       "version":"3.1.0","binary":"/srv/artifacts/proxy-amd64"}' \
  http://update.example.com:8080/admin/artifacts

# Retire a track. The bytes stay in the store until retention collects
# them, so this is cheap to undo.
curl -X DELETE \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://update.example.com:8080/admin/artifacts?artifact=keystone-proxy/linux/amd64"
```

Artifacts published this way persist across restarts via
`store.state_file`. Artifacts **declared in YAML** are re-published from
their `binary` path on every boot, so config always wins for the tracks
it names.

### `POST /admin/default`

Choose which track answers heartbeats that carry no `artifact` field.

```sh
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"artifact":"keystone-agent/linux/arm64"}' \
  http://update.example.com:8080/admin/default
```

### `POST /admin/gc`

Run a retention sweep immediately instead of waiting for the next tick.
Returns what it reclaimed. Answers `404` when `retention.enabled` is
false.

```sh
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://update.example.com:8080/admin/gc
# {"DeltasDeleted":12,"FullsDeleted":1,"BinariesDeleted":0,
#  "BytesReclaimed":48210944,"Scanned":40,"Duration":11000000}
```

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

- Old cached deltas (`{from}_{oldTarget}.delta.zst`) are never served
  after a reload: agents only ever request deltas toward a *current*
  target, so the new `target_hash` makes them unreachable. Enable
  `retention` (see below) to reclaim their disk space.
- The server caps body size (4 KiB) on heartbeat/report, panic-recovers
  every handler into a structured 500/5.00 response, and bounds
  concurrent bsdiff runs to protect CPU/RAM under bursty load from many
  distinct source versions.

---

## Artifacts (N components x M versions x K architectures)

One update-server serves any number of independently-versioned artifacts.
An **artifact** is a publication track identified by a `(name, os, arch)`
triple; the server holds exactly one current target per track.

```
keystone-agent/linux/arm64     <- one track
keystone-agent/linux/amd64     <- a different track, its own target
keystone-proxy/linux/arm64     <- another component entirely
tariff-table                   <- platform-independent: no os/arch
```

`os` and `arch` are optional but **coupled**: set both or neither. The
charset is deliberately narrow (`[A-Za-z0-9._-]` for names, `[a-z0-9]`
for platform tokens, no `.` or `..`), because keys reach HTTP paths,
retention bookkeeping and log fields.

Declare tracks in `server.yaml`:

```yaml
artifacts:
  - name: "keystone-agent"
    os: "linux"
    arch: "arm64"
    version: "1.4.2"
    binary: "/srv/artifacts/keystone-agent-linux-arm64"
  - name: "keystone-agent"
    os: "linux"
    arch: "amd64"
    version: "1.4.2"
    binary: "/srv/artifacts/keystone-agent-linux-amd64"
default_artifact: "keystone-agent/linux/arm64"
```

...or publish them at runtime through `POST /admin/artifacts`.

Each agent names its track in the heartbeat:

```yaml
device:
  artifact: "keystone-agent/linux/arm64"
```

An **empty** `artifact` resolves to `default_artifact`. That is what keeps
single-artifact deployments — and agents built before multi-artifact
support existed — working with no change at all. Likewise a bare
`target:` block in `server.yaml` is still accepted and folded into one
track named `default`.

### Why this costs almost nothing in storage

The store is **content-addressed**: binaries live at `{sha256}.bin` and
deltas at `{from}_{to}.delta.zst`. Those keys are globally unique, so two
tracks that happen to ship identical bytes share one file, and a delta is
identified by its pair regardless of which artifact requested it. Adding
artifacts multiplies *bookkeeping*, not *bytes*.

The registry — "which hash is current for which track" — is the only
per-artifact state, and it persists to `store.state_file` so anything
published through the admin API survives a restart.

### One Updater per artifact

`agent.Updater` follows exactly one artifact. An embedder that manages
several components runs one `Updater` per component: they can share a
`ProtocolClient`, but each needs its own slots directory and state
directory.

---

## Full-download fallback

Delta updates have a structural precondition: the server must still hold
the exact bytes the device is running. That fails for real devices in
ordinary ways —

- factory-flashed units the server has never seen,
- sideloaded or locally-built binaries,
- versions whose source binary aged out of retention,
- a device whose active slot is corrupt.

Previously such a device got `update_available: false` on every heartbeat,
forever: a silent, permanent strand that looks like a healthy fleet in the
logs. Now the server falls back to shipping the **whole target**, zstd
compressed, via `binary_endpoint` instead of `delta_endpoint`.

The agent's flow is nearly identical: verify the signature, download,
check the transfer hash, then **decompress instead of patching**. Notably
it never reads its own active slot on this path, which is why it also
recovers a device with a damaged binary.

The signature scheme is unchanged. The signed payload has always been
`(target_hash, hash-of-the-bytes-on-the-wire)`; in full mode those bytes
are the compressed target rather than a patch. See
[`docs/signing.md` §2.2](docs/signing.md) for the threat-model analysis,
including why a tampered mode flag fails safe.

Watch `updater_deltas_served_total{mode="full"}`: a rising full/delta
ratio means retention is evicting source binaries your fleet still needs,
and you should raise `retention.history_depth`.

To disable — only sensible when downlink is so scarce that a full transfer
is never acceptable *and* you handle stranded devices out of band:

```yaml
manifest:
  allow_full_download: false
```

---

## Retention (bounding disk growth)

Nothing in the store is ever overwritten, so without a sweeper every
release adds a binary plus one delta per source version still in the
field. Across N components x K architectures that growth is
multiplicative, and a full filesystem fails writes mid-campaign.

Retention is **off by default** — silently deleting an operator's
binaries is not a sensible default — and distinguishes two classes of
file:

| Class | Files | Policy |
|---|---|---|
| **Derived cache** | `{from}_{to}.delta.zst`, `{hash}.full.zst` | Collected aggressively. Deleting one costs CPU to regenerate and nothing else. |
| **Binaries** | `{hash}.bin` | The only copy of something you produced. Collected only with an explicit opt-in, only when unreferenced, and only after a grace period. |

```yaml
retention:
  enabled: true
  interval: "6h"
  history_depth: 10            # superseded targets kept as delta sources
  delta_max_age: "720h"        # 0 disables
  deltas_max_total_mb: 0       # 0 disables; oldest evicted first
  collect_orphan_binaries: false
  orphan_binary_min_age: "24h"
```

The rules, in the order they fire:

1. A delta whose **destination** is no longer any artifact's current
   target is unreachable by construction — agents only ever ask for
   deltas toward a current target — and is deleted.
2. A delta whose **source** is in no artifact's history is deleted:
   any device still on that version now takes the full-download path.
3. Anything older than `delta_max_age` is deleted.
4. If `deltas_max_total_mb` is exceeded, the oldest survivors are evicted
   until it fits.
5. With `collect_orphan_binaries`, binaries referenced by no artifact
   (current target or history) and older than `orphan_binary_min_age` are
   deleted.

Two properties worth internalising:

- **Collecting a binary never strands a device.** An agent still running
  it falls back to a full download. The cost is downlink, not
  availability — which is why `history_depth` is a tuning knob and not a
  correctness requirement.
- **The sweeper only touches files it can parse.** A stray note, a temp
  file, or a future format in `binaries_dir`/`deltas_dir` is left alone
  and logged at DEBUG. Both hash segments are validated as SHA-256 hex,
  so a crafted filename cannot widen what gets deleted.

The `orphan_binary_min_age` grace period exists for a specific race:
`RegisterBinary` writes the file before the `Publish` that references it,
and a sweep landing between the two would delete a binary seconds away
from becoming a target.

Run one on demand with `POST /admin/gc`; watch
`updater_retention_*` for what it reclaims.

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

    "github.com/carlosprados/ota-updater/pkg/agent"
    "github.com/carlosprados/ota-updater/pkg/crypto"
    "github.com/carlosprados/ota-updater/pkg/protocol"
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

## Update cadence jitter

Every `RunOnce` cycle, the agent sleeps `CheckInterval ± (Jitter × CheckInterval)`
before the next iteration. With the default `check_interval: 1h` and
`jitter: 0.3`, each sleep is a uniform sample in `[42 min, 78 min]`. This
matters for fleet behavior:

| Scenario | Without jitter | With ±30% jitter |
|---|---|---|
| 5 000 devices deployed at 14:00 | 5 000 req/s every 15:00:00, 16:00:00 … | ≈2.3 req/s spread over a 36-min window |
| Fleet-wide reboot after a brief outage | All agents re-synchronize, pattern repeats | Decays in 3–5 cycles |

The jitter is **re-sampled every cycle**, so any accidental synchronization
(fleet reboot, mass deployment) fades away naturally. It does NOT replace
staggered rollouts on day zero (use deployment automation for that) nor
rate limiting on the server (still pending), but it keeps steady-state
traffic flat enough that a modest server handles a large fleet.

Set `update.jitter: 0` to disable (lock-step cadence — **not recommended**
past a handful of devices). Valid range `[0, 1]`.

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

## Observability

A separate HTTP listener, configurable in `server.yaml`, exposes Prometheus
metrics and (optionally) `net/http/pprof`. Bind it to loopback or a
private network — `/metrics` has no authentication and `/debug/pprof`
reveals process internals.

```yaml
metrics:
  addr: "127.0.0.1:9100"   # empty string disables the listener entirely
  pprof_enabled: false     # flip to true only while actively debugging
```

### Metrics (prefix `updater_`)

The server publishes standard `go_*` / `process_*` collectors plus:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `updater_heartbeats_total` | counter | `transport`, `result` | `result` ∈ `none` · `update` · `retry` · `bad_request` · `error`. |
| `updater_heartbeat_duration_seconds` | histogram | `transport` | Server-side wall time per heartbeat. |
| `updater_deltas_served_total` | counter | `transport`, `hot_hit`, `mode` | `hot_hit` ∈ `hit`/`miss` — watch it to size `hot_delta_cache_mb`. `mode` ∈ `delta`/`full`; a rising `full` share means retention is evicting source binaries the fleet still needs. |
| `updater_delta_serve_duration_seconds` | histogram | `transport`, `mode` | Includes hot-miss disk reads. |
| `updater_delta_generations_total` | counter | `result` | `ok` / `error`. |
| `updater_delta_generate_duration_seconds` | histogram | — | Wall time of bsdiff runs (bounded by `delta_concurrency`). |
| `updater_async_generations_inflight` | gauge | — | Concurrent bsdiff goroutines. Sustained high = bump concurrency or investigate thundering herds. |
| `updater_manifest_cache_entries` | gauge | — | Signed-manifest LRU occupancy. |
| `updater_hot_delta_cache_bytes` | gauge | — | Current bytes held in the hot delta LRU. |
| `updater_hot_delta_cache_entries` | gauge | — | Hot delta LRU occupancy. |
| `updater_target_cache_bytes` | gauge | — | Bytes of uncompressed delta-target binaries held in RAM, across all artifacts. |
| `updater_target_cache_entries` | gauge | — | How many targets are resident. |
| `updater_artifacts` | gauge | — | Registered publication tracks. |
| `updater_artifact_target_size_bytes` | gauge | `artifact` | Uncompressed size of each track's current target. Cardinality is bounded by the number of artifacts, not by traffic. |
| `updater_retention_sweeps_total` | counter | `result` | `ok`/`error` per sweep. |
| `updater_retention_deleted_files_total` | counter | `kind` | `delta`/`full`/`binary`. |
| `updater_retention_reclaimed_bytes_total` | counter | — | Disk reclaimed by the sweeper. |
| `updater_admin_requests_total` | counter | `endpoint`, `code` | `code` collapses to `2xx`/`3xx`/`4xx`/`5xx` plus explicit `Unauthorized`/`Forbidden`/`Too Many Requests`. |
| `updater_admin_rate_limited_total` | counter | — | 429s emitted by the admin token-bucket. Non-zero on steady state = someone hammering. |
| `updater_signature_failures_total` | counter | — | Manifest sign errors. Should stay at 0. |

### `/debug/pprof`

`pprof_enabled: true` adds `/debug/pprof/{profile,heap,goroutine,...}`:

```sh
go tool pprof -http=: http://127.0.0.1:9100/debug/pprof/profile?seconds=30
go tool pprof -http=: http://127.0.0.1:9100/debug/pprof/heap
curl http://127.0.0.1:9100/debug/pprof/goroutine?debug=2
```

Leave it off in production. Tactical tool, not permanent infrastructure.

### Disk-space warning at startup

Both server and agent log a WARN at startup if the filesystem holding
their state is below either threshold (percentage OR absolute MB). The
server checks `binaries_dir` and `deltas_dir`; the agent checks
`slots_dir`. Defaults 10% / 100 MiB. `0` on either disables that check.

Server YAML:
```yaml
store:
  disk_space_min_free_pct: 10
  disk_space_min_free_mb: 100
```

Agent YAML:
```yaml
device:
  disk_space_min_free_pct: 10
  disk_space_min_free_mb: 100
```

Purely observational — the service boots regardless. The warning lands
in the same log stream with `op=disk_space`.

---

## Memory limits (device-side)

Both binaries are pure-Go static binaries that, by default, assume they
can use all RAM of the host. On NB-IoT devices with ≤512 MiB of RAM
shared with other services, or on containerised deployments with cgroup
caps, you want **two knobs working together** to avoid OOM kills:

### `GOMEMLIMIT` (Go runtime, 1.19+) — soft limit

Tells the Go runtime "the heap must stay under this". When the heap
approaches the limit, the GC runs **more aggressively** to reclaim
memory; the process does not die. Set this as an environment variable:

```sh
GOMEMLIMIT=200MiB ./bin/edge-agent -config /etc/agent.yaml
```

This is a *soft* limit. If the process genuinely needs more memory (big
bsdiff generation on server, big delta patch on agent), the GC will
burn CPU trying to stay under the limit but the workload eventually
succeeds. The trade-off is "memory budget over CPU usage", which is
exactly the right call on a constrained device.

### `MemoryMax=` (systemd) / `--memory=` (docker) — hard limit

Applied by the kernel cgroup, enforced by the OOM killer. If the
process goes over, the kernel kills it. This is the *last defense*;
without it, a runaway process can starve the rest of the system.

### The combo

Always set both, with `GOMEMLIMIT` at ~**80% of `MemoryMax`** so the Go
runtime has a window to react before the kernel pulls the trigger.

Example systemd unit for the edge-agent:

```ini
[Unit]
Description=OTA edge agent
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/edge-agent -config /etc/edge-agent.yaml
Environment="GOMEMLIMIT=200MiB"
MemoryMax=256M
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Docker / Kubernetes equivalents:

```yaml
# docker-compose
services:
  edge-agent:
    image: your/edge-agent:1.0.0
    environment:
      - GOMEMLIMIT=200MiB
    mem_limit: 256m

# kubernetes Pod spec
env:
  - name: GOMEMLIMIT
    value: "200MiB"
resources:
  limits:
    memory: "256Mi"
```

The server has analogous needs but with a larger budget because bsdiff
peaks at ~20× the target binary size. For a 20 MiB target at
`delta_concurrency: 2` reserve at least `GOMEMLIMIT=1GiB` with
`MemoryMax=1280M`.

### What happens without these knobs

- **No cgroup, no GOMEMLIMIT** (bare metal, low-RAM device): Go grows
  until `malloc` fails. In practice the kernel OOM kills something —
  often not you, which is worse (random service dies, your log says
  nothing).
- **cgroup set, no GOMEMLIMIT**: Go keeps expanding because it thinks
  it has all of host RAM. Cgroup OOM fires from "out of the blue";
  systemd/Docker restarts the process and the cycle repeats until the
  natural heap peak passes.
- **GOMEMLIMIT but no cgroup**: if anything escapes the Go heap
  (cgo, unexpected stack growth, kernel pinning on network buffers),
  nothing stops it. Less common but still possible.

---

## Memory bounds (server, 24/7 operation)

The update-server is designed for **strictly bounded RAM** regardless of
the size of its history *or the number of artifacts*: 100 binaries or
10 000, one track or fifty, the memory footprint is governed by three
knobs in `configs/server.yaml` and nothing else.

```yaml
store:
  target_cache_mb: 200        # delta-target binaries in RAM, ALL artifacts
  hot_delta_cache_mb: 512     # hot LRU of transfer bytes
  delta_concurrency: 2        # max concurrent bsdiff runs

manifest:
  cache_size: 4096            # signed-manifest LRU entry count
```

### What is held in RAM

| Item | Bound | Notes |
|---|---|---|
| Delta-target binaries | ≤ `target_cache_mb` | Byte-budget LRU keyed by hash, **shared across all artifacts** — this is what stops N tracks from multiplying resident memory. A target larger than the whole budget is never cached and is re-read from disk per generation. |
| Source binaries (historical) | **0** | Never held. Read from disk at each bsdiff generation; the kernel page cache handles OS-level LRU. |
| Hot transfer cache | ≤ `hot_delta_cache_mb` | Byte-budget LRU holding both deltas and whole compressed binaries. Values bigger than the whole budget are rejected (never cached). |
| Signed manifests | ≤ `cache_size` entries × ~500 B | Entry-count LRU keyed by `(artifact, from, to)`. |
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

- **`target_cache_mb`** — set to the biggest target you plan to ship
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

## Operational recommendations for unattended deployments

If your fleet is deployed somewhere you can't easily reach — rural
NB-IoT sensors, remote industrial equipment, devices buried in
infrastructure — you need to think about **long-term reliability**,
not just "does the first update work". This section is the honest
answer to "can I trust this for 100 updates over a year on a device
I cannot service physically?".

### The self-replacement mechanism does not degrade

`syscall.Exec` — the default `RestartStrategy` — compiles down to
`execve(2)`, which has no accumulating state:

- The whole virtual address space is released (mappings, heap, stack).
- All file descriptors with `CLOEXEC` are closed — Go marks `CLOEXEC`
  by default on everything it opens (since Go 1.12), so no FD leak
  crosses the exec boundary.
- Signal handlers reset to defaults.
- `argv` and `envp` are replaced atomically.

Iteration 100 starts from the same clean floor as iteration 1. There
is no memory fragmentation, no descriptor build-up, no counter that
grows without bound. `systemd`, `init(1)` and every shell rely on
this guarantee daily.

### Real long-term failure modes, ranked by risk

1. **The new binary crashes before its first heartbeat** — panic in
   `init()`, config parse error, missing dependency, bad file
   permission. Our watchdog is heartbeat-based, so if the process
   dies before issuing one, the watchdog never runs. Mitigation:
   the persistent `boot_count` at `<slotsDir>/.boot_count` survives
   process death; once it exceeds `MaxBoots` (default 2), the agent
   performs a **permanent rollback** to the previous slot on the
   next boot. This only works if an external supervisor relaunches
   the failed binary — see "Supervisor is mandatory" below.

2. **Disk exhaustion from write residue** — partial downloads,
   `.tmp-*` files, or unrotated logs. The agent sweeps
   `.tmp-*`/`.partial` older than 24 h in `<StateDir>` on every
   boot (and the server does the same for `binaries_dir` and
   `deltas_dir`). Disk-space warnings at startup — see "Disk-space
   warning at startup" — give you a Prometheus-visible hint before
   things go wrong. You still need to configure log rotation in
   your supervisor (journald defaults to 10 % of the disk; on a 4
   GB device that is 400 MB of volatile state).

3. **Flash wear** — 100 updates/year × 12 MB per binary ≈ 1.2 GB
   written per slot per year. Consumer eMMC handles 3 000–10 000
   program/erase cycles per cell, so this is years of headroom.
   In practice NB-IoT fleets push 4–12 updates per year, not 100.
   Essentially a non-issue.

4. **Both slots hold a bad version** — A/B + boot-count rolls you
   back to the last known-good slot, but if the bug was already
   present there, you are stuck. This is a deployment-process
   problem, not a mechanism problem. See "Canary your deploys"
   below.

5. **Ed25519 public key file corruption or rotation** — if the
   public key on the device becomes unreadable, the agent can no
   longer verify any manifest and freezes on its current version.
   Writes are atomic and `fsync`'d (see `pkg/atomicio`), but a
   physically dying flash sector could still cause this. Consider
   storing the trust-anchor key on a read-only partition, or
   keeping a backup copy at a second path that the agent can fall
   back to.

6. **A bug in the agent itself gets published over-the-air** — if
   the new `edge-agent` survives the first confirm cycle but
   degrades later (for example, a bug that surfaces only after 48 h
   of uptime), no automatic rollback will catch it. Again, this is
   a deployment problem; the mechanism works as intended.

### What is already in place

- Signature verified **before** the delta is downloaded — a tampered
  or corrupt manifest never causes wasted NB-IoT downlink and never
  writes garbage to the inactive slot.
- Watchdog with `N=3` heartbeat attempts within `WatchdogTimeout`,
  tolerant to transient NB-IoT connectivity loss.
- Permanent rollback after `MaxBoots=2` boots without a confirm.
- All on-disk writes go through `pkg/atomicio` with
  `fsync`+`rename`+`fsync(dir)` — power-loss safe.
- `ENOSPC` triggers a 5 min backoff instead of burning retries on
  a static condition.
- `ExecRestart` failure writes a persistent cooldown file (1 h by
  default, tunable via `update.restart_failure_cooldown`) so that
  a broken new binary can't trap the process in an `exec`-fail
  loop that starves the CPU.
- Admin endpoints are rate-limited with a minimum-length token
  enforcement at boot.

### Supervisor is mandatory for unattended deployments

**If the device is somewhere you cannot physically reach, deploy
the agent under an external supervisor.** No exceptions. Failure
mode #1 above — early panic in the new binary — is mitigated by
the persistent boot counter *only if something relaunches the
failed binary*. Recommended supervisors:

- **systemd**: `Restart=always`, `RestartSec=5s` is enough. Point
  `ExecStart=` at the `current/edge-agent` symlink, not the slot
  directly, so that rollback is transparent to the unit. See the
  "systemd" section above for a full unit file.
- **Docker**: `restart: unless-stopped` or `always`. Again, the
  entrypoint should resolve the active slot via symlink. See the
  "Docker" section above.
- **Bare shell wrapper** (minimal devices without either): a
  `while true; do $bin; sleep 5; done` loop will do the job. Not
  pretty, but the contract (relaunch on exit) is what matters.

### Canary your deploys

A/B rollback protects each device against a bad update. It does
**not** protect the fleet against a bad build. Before rolling a
new version to all devices:

1. Publish it to an `update-server` serving only a canary subset
   (1–10 % of the fleet, ideally geographically and functionally
   diverse).
2. Observe for 24–48 h. Key signals: `updater_heartbeats_total`
   staying steady per device, no spike in
   `updater_signature_failures_total` (indicates key/config drift),
   no rise in "boot count exceeded" reports arriving at the server.
3. Only then promote the same binary to the production
   `update-server` and let the rest of the fleet upgrade.

Staged-rollout logic in the server itself (returning different
manifests based on a device cohort) is **not implemented**; it
would be a future extension. Today you achieve the same effect by
running two `update-server` instances with different target
binaries.

### Monitoring signals worth alerting on

From the `/metrics` endpoint (see "Observability"):

- `updater_heartbeats_total{result="fail"}` — a rise means devices
  are failing to talk to the server; could be network or
  could be a new build that can't even report home.
- `updater_signature_failures_total` — should be zero. Any non-zero
  value implies key drift, clock-skew on your build pipeline, or
  tampering.
- `updater_admin_rate_limited_total` — a spike suggests somebody is
  brute-forcing the admin bearer token.
- Device-reported `UpdateReport` messages with `success=false` —
  these surface in server logs as WARN with the failure reason;
  they are your first warning that a rollout has gone wrong.

### TL;DR

- The `exec` mechanism itself is reliable at any iteration count;
  it does not degrade.
- Long-term failure modes are **operational**, not mechanical, and
  are mitigated by: a mandatory external supervisor, disciplined
  canary deploys, and monitoring the signals above.
- Without an external supervisor, this agent should not be
  deployed to devices you cannot physically reach. With one, plus
  a canary workflow, it is designed for this exact use case.

---

## Known limitations

Items that were analysed and consciously deferred. None of these block
24/7 operation at the fleet sizes this project targets; they are
tracked so the next iteration starts from a clear baseline.

- **CoAP delta downloads do NOT support resume from a non-zero offset.**
  If a Block2-transferred delta is interrupted mid-download, the agent
  discards its `.partial` state and restarts the transfer from byte 0.
  HTTP delta downloads resume normally via Range requests. For small
  deltas (under ~100 KiB) the wasted NB-IoT downlink is negligible;
  for larger payloads prefer HTTP as the primary transport, or accept
  the cost when CoAP is mandatory for your radio stack. Implementing
  CoAP Block2 start-at-N would require lower-level access to
  `go-coap` options and is tracked as a follow-up PR.

- **Admin rate limit is global, not per-IP.** See the "Admin control
  plane" section above for the operational mitigation.

- **Clock skew of `Heartbeat.Timestamp` is logged but not validated.**
  The field is advisory — signatures and version authenticity live in
  the manifest, not in the heartbeat. If clock-skew monitoring becomes
  operationally useful, the scaffolding to add a warn + Prometheus
  metric is ~40 LOC. Enforcement (rejection) would not be added even
  then, because it would block updates to devices whose RTC is
  corrupted — exactly the ones that need the update.

- **bsdiff is the delta algorithm**. Memory peaks at ~20× the target
  binary during generation. For targets much beyond 100 MiB the server
  needs either more RAM or a different algorithm. librsync was
  evaluated and rejected (delivers deltas ~100× larger than bsdiff on
  realistic Go binaries); see `benchmark/` for the harness. xdelta3
  is the next candidate if this limit becomes real.

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
  server/            # server-side: content-addressed Store, artifact Registry,
                     #   Manifester, HTTP+CoAP handlers, admin, Retention
  protocol/          # wire types (JSON + CBOR) shared by HTTP and CoAP
  crypto/            # Ed25519 sign/verify + PEM key I/O
  delta/             # bsdiff + zstd patch pipeline
  compression/       # zstd wrappers
  atomicio/          # durable writes: fsync + atomic rename + parent fsync
integration/         # //go:build integration end-to-end test
tools/keygen/        # Ed25519 keypair generator CLI
docs/
  signing.md         # authoritative signature scheme reference
configs/             # example agent.yaml and server.yaml
Taskfile.yml         # build/test/ci task runner config
```

---

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

```
Copyright 2026 Carlos Prados

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```
