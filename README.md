# OTA Updater

Lightweight OTA (Over-The-Air) update system in Go, tuned for **NB-IoT
constrained networks** (~20-60 kbps, high latency, frequent disconnects).
Ships two binaries plus a keygen tool.

> **Status — work in progress.** This README reflects the current subset of
> the 18-step implementation plan. It will be completed at step 18. See
> `CLAUDE.md` for the living checklist.

---

## Components

| What | Where | Role |
|---|---|---|
| `update-server` | `cmd/update-server/` *(pending, step 9)* | Serves signed manifests and compressed delta patches over HTTP and CoAP. |
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

> The auto-reload + admin endpoint are implemented at step 9. Until
> that step lands, reload means a server restart.

---

## Admin endpoint: `POST /admin/reload`

Explicit reload trigger, useful in CI/CD pipelines that want a
synchronous confirmation rather than waiting for the filesystem watcher.

**Authentication: static Bearer token.**

- Token is configured in `server.yaml` under `admin.token`.
- Requests must carry `Authorization: Bearer <token>`.
- Server compares tokens in constant time.
- Mismatch or missing header → `401 Unauthorized`.

### Example

```sh
curl -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://update.example.com:8080/admin/reload
```

### Response

- `200 OK` with JSON `{"target_hash": "<sha256-hex>"}` on success.
- `401 Unauthorized` on auth failure.
- `500 Internal Server Error` if the reload itself failed (for example,
  the target file is missing or unreadable). The previous target
  remains active — the server never ends up in a broken state.

### Operational notes

- The endpoint is intended for **internal networks or loopback**.
  Expose it only where you can control who reaches it. Combine with a
  reverse proxy + ACL if needed.
- Rotating the token: edit `server.yaml`, then call `/admin/reload`
  yourself with the new token — the config is re-read as part of the
  reload path. (Rotation details finalized at step 9.)
- Old cached deltas (`{from}_{oldTarget}.delta.zst`) remain on disk
  after a reload but are never served: the new `target_hash` makes
  them unreachable. Prune manually or via future tooling.

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

## Device-side embedding (library usage)

**Planned for step 18.** The agent packages (`internal/agent/…` today)
will be promoted to `pkg/agent/` so any Go binary can embed self-update
behavior:

```go
// Sketch — API subject to change before step 18.
updater := agent.New(agent.Config{ /* … */ })
if err := updater.Run(ctx); err != nil {
    log.Fatal(err)
}
```

`cmd/edge-agent` will become a thin wrapper around this library.
Full example in this README at step 18.

---

## Implementation progress

Tracked as a checklist in `CLAUDE.md` (18-step plan from
`prompt-ota-updater.md`). At the time of writing, steps 1-7 are
complete; steps 8-18 are pending.

---

## Repo layout

```
cmd/            # main packages for the two binaries (pending)
docs/           # long-form design docs (signing, …)
internal/
  agent/        # device-side logic (pending)
  compression/  # zstd wrappers
  crypto/       # Ed25519 sign/verify + PEM key I/O
  delta/        # bsdiff + zstd patch pipeline
  protocol/     # wire types (JSON + CBOR) shared by HTTP and CoAP
  server/       # store, manifester, HTTP handler (CoAP pending)
tools/keygen/   # Ed25519 keypair generator CLI
```
