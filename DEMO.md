# DEMO — OTA updater in 30 minutes

> Spanish version: [DEMO.es.md](DEMO.es.md).

Operational walkthrough for showing the system to teammates. It mixes
the narrative (what you say) with the exact commands (what you run).
Readable straight through on a second screen while you project the
first one, or as a cheat-sheet when you want to improvise knowing every
command is documented.

**Prerequisites on the host**
- Go 1.22+ (tested on 1.25).
- [Task](https://taskfile.dev) installed (`go install github.com/go-task/task/v3/cmd/task@latest`).
- A browser — ideally one you can project full-screen.
- Bruno (optional but recommended): GUI at <https://www.usebruno.com/downloads>, or CLI `npm i -g @usebruno/cli`.

**Duration**: 25–35 min with questions. The rollback section (step 9)
is the first thing to drop if you're tight on time.

---

## 0 · The one-minute pitch

> "We have thousands of NB-IoT devices in the field. Updating them is
> expensive (the radio window costs money and battery) and risky (a
> broken binary can brick a device out of reach). This OTA system
> addresses both: it ships only the **signed binary delta** between the
> old and the new version (kilobytes, not megabytes), keeps **two A/B
> slots** so the new binary never overwrites the old one until it has
> proven it boots healthy, and **rolls back on its own** if it doesn't.
> One server, plus an agent **that can be embedded as a Go library**
> inside any binary."

---

## 1 · Architecture

```text
                                            fsnotify
                            target.bin  ─────────────┐
                               │                     │
                               │                     ▼
  ┌───────────────────┐    POST /heartbeat    ┌───────────────────┐
  │                   │───────────────────────▶                   │
  │   edge-agent      │    GET /delta/A/B     │   update-server   │
  │   (device)        │◀──────────────────────│   (datacenter)    │
  │                   │    POST /report       │                   │
  └───────────────────┘───────────────────────▶                   │
       │                                      └───────────────────┘
       │ a/b swap + watchdog                        │  │
       ▼                                            │  │  /metrics
   ┌───┴───┐                                        │  │  (prometheus)
   │ slot A│ ◀─── active symlink                    │  │
   │ slot B│                                        │  │  /admin/reload
   └───────┘                                        │  │  /admin/loglevel
                                                    │  │  (bearer token)
                                                    ▼  ▼
                                               operator / CI / ops
```

**Ports used by the demo (all loopback)**:

| Port | Component | Role |
|---|---|---|
| `127.0.0.1:18080` | update-server | OTA API: `/heartbeat`, `/delta/{from}/{to}`, `/report`, `/health`, `/admin/*` |
| `127.0.0.1:15683` | update-server | CoAP UDP (available but not exercised in this demo) |
| `127.0.0.1:19100` | update-server | Observability: `/metrics` (Prometheus) + `/debug/pprof/*` |
| `127.0.0.1:7000`  | demo app | The deployed "application"'s own HTTP page — this is the thing that visibly changes when an update lands |

**What each binary is**:

- `update-server` — runs in the datacenter. Stores binaries and deltas,
  signs manifests with Ed25519, serves HTTP and CoAP.
- `demo/apps/v{1,2,3}/` — three versions of *the same sample product*.
  Each binary is an HTTP server on `:7000` with a visibly distinct page
  (colours + text + endpoints) **AND embeds `pkg/agent`** as a library
  to self-update. This is the key point: **auto-update is not a
  separate process, it's part of your own application**.

---

## 2 · Installing Bruno (optional, 2 min)

Bruno is a Postman-style API client whose requests live as plain text
files next to the code. The collection is already at `bruno/` in this
repo.

**GUI (recommended for the demo)**:
1. Download from <https://www.usebruno.com/downloads>.
2. Open Bruno → _Open Collection_ → pick this repo's `bruno/` folder.
3. In the top bar, select the "**demo**" environment.
4. Click the environment's gear icon → _Secrets_ tab → fill
   `admin_token` with:

   ```
   demo-token-please-replace-in-prod-0000
   ```

   (same value as in `demo/configs/server.yaml`).

**CLI (skip the GUI)**: `npm i -g @usebruno/cli` then
`bru run bruno/public/health.bru --env demo`.

---

## 3 · Setup (once, before you start)

Builds the binaries, generates the Ed25519 keys, seeds the initial
state:

```sh
./demo/setup.sh
```

This populates `/tmp/ota-demo/` with:
- `update-server` binary,
- `apps/v1`, `apps/v2`, `apps/v3` binaries,
- `keys/server.key` + `keys/agent.pub`,
- `target.bin` pointing to v1,
- `agent/slots/A` and `agent/slots/B` seeded with v1,
- `agent/current` symlink → `slots/A`.

To wipe everything at any time: `./demo/cleanup.sh`.

---

## 4 · Start the server (terminal 1, ~2 min)

```sh
./demo/run-server.sh
```

You'll see in the logs:

```
store opened target_hash=... target_in_memory=true ...
target watcher started dir=/tmp/ota-demo file=target.bin ...
coap listening addr=127.0.0.1:15683
http listening addr=127.0.0.1:18080
observability listening addr=127.0.0.1:19100 pprof=true
```

> **What to tell the room**: "The server has read the target binary,
> computed its SHA-256, and kept it in memory. It opened three ports:
> the OTA API, CoAP for heavily constrained devices, and a separate
> observability port for Prometheus. Public API and metrics never share
> a socket."

From Bruno (or `curl`), check liveness:

```sh
curl -s http://127.0.0.1:18080/health
```

**In Bruno**: `public/health` → _Run_.

```json
{"status":"ok","target_hash":"3c03ef60..."}
```

---

## 5 · Start the demo app (terminal 2, ~2 min)

```sh
./demo/run-app.sh
```

The script executes `/tmp/ota-demo/agent/current` (symlink → `slots/A`,
which is v1). You'll see:

```
demo banner HTTP listening op=demo version=1.0.0 addr=127.0.0.1:7000
demo app ready version=1.0.0 device_id=demo-device-01
no pending update; entering steady state
```

> **What to tell the room**: "What just started is **not** a separate
> agent, it's our sample application. It carries the `pkg/agent`
> library inside. In a real scenario this would be your production
> binary: an IoT gateway, a telemetry agent, a probe. The auto-update
> is embedded — no extra infrastructure to deploy."

**Open the browser and project it**: <http://127.0.0.1:7000/>

Blue page, big text "v1.0.0 — Initial release". The page auto-refreshes
every 2 s (see the `<meta http-equiv="refresh">`).

Every 5 s the agent log shows:

```
heartbeat served from=<hash> to=<hash> update_available=false
```

> "The device asks the server every 5 s. Same hash → nothing to do. In
> production the cadence is 1 hour with ±30 % jitter so 5 000 devices
> don't hit the same second."

---

## 6 · Publishing v1.1.0 (minor update, ~3 min)

In a **third** terminal (server and app keep running):

```sh
./demo/publish-version.sh v2
```

This overwrites `/tmp/ota-demo/target.bin` with the v1.1.0 binary via
an atomic `cp` + `mv`.

**In the server logs (terminal 1)** almost immediately:

```
target event event=RENAME name=target.bin
store target reloaded previous_hash=<old> target_hash=<new> target_in_memory=true
```

> "The server's fsnotify saw the file change, read the new binary,
> computed its SHA-256 and swapped the active target. Any heartbeat
> from now on will get the new manifest."

**In the agent logs (terminal 2)**, within 5 s:

```
heartbeat served update_available=true retry_after=5          # server spawning bsdiff
delta not yet ready retry_after=5
...
download complete path=/tmp/ota-demo/agent/slots/.staging.delta size=... transport=http
inactive slot written slot=B path=/tmp/ota-demo/agent/slots/B
active slot swapped new_active=B target=/tmp/ota-demo/agent/slots/B
update applied; restarting exec=/tmp/ota-demo/agent/slots/B
# ← here the process is replaced via syscall.Exec; same PID
demo banner HTTP listening op=demo version=1.1.0 addr=127.0.0.1:7000
post-swap boot detected; entering watchdog window
health check passed attempt=1 of=3
watchdog confirmed healthy boot
update confirmed; steady state engaged
```

**Look at the browser**: the page flips blue → **green**, `<h1>` says
**v1.1.0**, and a yellow "NEW — try GET /hello" badge shows up.

Hit the new endpoint:

```sh
curl http://127.0.0.1:7000/hello
# Hello from v1.1.0! Endpoint introduced in the 1.1.0 release.
```

> **What just happened in 11 steps**:
> 1. Server saw the new target via fsnotify.
> 2. Agent heartbeat, got "update available".
> 3. Server hadn't built the delta yet → replied `RetryAfter=5`.
> 4. In the background the server ran `bsdiff` + `zstd` between v1.0.0 and v1.1.0.
> 5. Next heartbeat: server signs the manifest with Ed25519 and returns it.
> 6. Agent VERIFIES the signature **before downloading** (if tampered, aborts with zero bytes transferred).
> 7. Downloads the compressed delta (~250 KiB between two ~11 MiB Go binaries).
> 8. Applies bsdiff + zstd → new binary → writes it to the inactive slot (B).
> 9. Flips the symlink → B is now active.
> 10. Writes `.pending_update`, calls `syscall.Exec` on the new binary → process image is replaced, PID and open sockets survive, HTTP `:7000` restarts on the new code.
> 11. New binary boots, sees `.pending_update`, runs a health-check heartbeat, **confirms** → settles.

### Bonus: why didn't the agent's process die?

You may notice something counterintuitive: you never `Ctrl-C`'d the
agent, you never started a new one, and yet the banner now serves
v1.1.0 and the logs keep streaming in the *same* terminal. How is
that possible after an update?

Because the agent **doesn't fork-and-exec**, it **`syscall.Exec`s**.
The kernel replaces the process image — code segment, data, heap —
with the new binary, but the process itself is never destroyed:

- **Same PID**, same PPID — so your shell never received a `SIGCHLD`
  and believes its child is still alive and well.
- **Same cgroup, same uid/gid, same working directory, same env**.
- **All open file descriptors survive**, including `stdin/stdout/stderr`
  attached to your terminal — that's why the logs keep flowing.

It's a deliberate design choice (default is `ExecRestart` in
`pkg/agent`). It makes self-update transparent to:

- `systemd` (any `Type=`, including `notify`),
- Docker (PID 1 never changes, so the container stays up),
- and — as you're seeing right now — an interactive shell.

**Prove it in a fourth terminal**, while the demo is running:

```sh
PID=$(pgrep -f edge-app)
echo "PID: $PID"
readlink /proc/$PID/exe

# now publish v2.0.0 and watch terminal 2 do its thing:
./demo/publish-version.sh v3

# re-run the same two commands:
echo "PID: $PID"                    # → SAME number
readlink /proc/$PID/exe             # → points to the OTHER slot now
```

The PID hasn't moved, but `/proc/<pid>/exe` is now pointing at a
different binary in the other A/B slot. Literally the same process
running different code.

> The alternative strategy `ExitRestart` (shipped but not the default)
> has the process actually exit with status 0 and rely on a
> supervisor — `systemd`, Docker `restart: always` — to bring it
> back. Useful when your deployment treats processes as cattle, but
> it would **not** survive a bare shell like the one in this demo.

---

## 7 · Major bump v2.0.0 (another 3 min)

```sh
./demo/publish-version.sh v3
```

Same sequence. Page turns **red**, `<h1>` reads "v2.0.0", new endpoint
`/status`:

```sh
curl -s http://127.0.0.1:7000/status | python -m json.tool
```

```json
{
  "go": "go1.25.6",
  "hostname": "imperator",
  "introduced": "2.0.0",
  "pid": 620739,
  "uptime_secs": 12,
  "version": "2.0.0"
}
```

Note that **the PID has not changed** across the demo — it's the same
process image, replaced three times. Compare with the pid at start.

---

## 8 · Observability (3 min)

**In Bruno**: `metrics/scrape` → _Run_. Or with curl:

```sh
curl -s http://127.0.0.1:19100/metrics | grep '^updater_' | sort
```

Series worth narrating:

```
updater_heartbeats_total{result="update",transport="http"}        # 2
updater_deltas_served_total{hot_hit="hit",transport="http"}        # 2
updater_delta_generations_total{result="ok"}                       # 2
updater_heartbeat_duration_seconds_count{transport="http"}         # N
updater_hot_delta_cache_bytes                                      # 5e5..
updater_target_binary_size_bytes                                   # ~11e6
updater_target_in_memory                                           # 1
```

> "Each line has clear operational meaning. In production, if
> `updater_async_generations_inflight` stays high, something is
> requesting bsdiff over and over — worth investigating. If
> `deltas_served_total{hot_hit='miss'}` dominates over `hit`, the hot
> cache is undersized."

If you want to show `pprof`:

```sh
go tool pprof -http=: http://127.0.0.1:19100/debug/pprof/goroutine
```

(Opens a local UI with the goroutine graph.)

---

## 9 · Bonus: simulated rollback (3 min, optional)

To show the watchdog saving the device: corrupt the target before
publishing so the new process can't boot:

```sh
# sabotage: publish a "binary" that isn't executable
echo 'not a binary' > /tmp/ota-demo/target.bin
chmod -x /tmp/ota-demo/target.bin
```

In the next cycle:
- The server hands out a manifest with `target_hash` = hash of "not a binary\n".
- The agent downloads the "delta", applies it → bsdiff fails → hash mismatch → **aborts without swapping**.

For a more visual angle, show that **before** downloading the agent
verifies the manifest signature. If the server returned a tampered
manifest (e.g. a wrong deltaHash signed by another key),
`crypto.Verify` fails and the agent downloads **zero bytes**.

> "That's the real value: the agent never spends radio on bytes it
> didn't authenticate first. It's the deliberate design call that
> saves NB-IoT downlink when the pipeline misbehaves."

To restore and keep playing:

```sh
./demo/publish-version.sh v1   # back to the initial blue
```

---

## 10 · Wrap up

On terminals 1 and 2: `Ctrl+C`. Logs show the ordered shutdown:

```
shutdown requested
target watcher stopping
store closed cleanly
shutdown complete
```

To fully wipe the state (deletes `/tmp/ota-demo/`):

```sh
./demo/cleanup.sh
```

---

## Appendix · Questions that usually come up

**Why no HTTPS?**
By design: NB-IoT devices sometimes don't have the resources for TLS.
**Authenticity** of the binary lives in the Ed25519 manifest signature,
not in the channel. **Confidentiality** isn't a requirement here: an
attacker only sees compressed, signed bytes they can't modify without
invalidating the signature.

**Why bsdiff and not something "more modern"?**
We evaluated librsync and its deltas are ~100× larger than bsdiff's on
real Go binaries. See `benchmark/` in the repo for the numbers.
xdelta3 is the next candidate if we ever need targets >100 MiB.

**What if the device loses track of time?**
The heartbeat carries a `timestamp` but the server **does not validate
it**. It's advisory. Authenticity and integrity live in the manifest
signature, not in the timestamp.

**What happens if the agent crashes mid-download?**
The `.partial` stays on disk with its JSON state. On next start, HTTP
resumes with `Range` from the correct byte. CoAP in this release
restarts from zero (known limitation in README).

**How many devices can this server handle?**
Stated target: **thousands** of NB-IoT agents. RAM is strictly bounded
(configurable LRU caches), async goroutines are tracked, and the admin
surface is rate-limited. In steady state the server sits around 20 MiB
RSS. Tens of thousands would need a second replica and shared storage;
not the current case.

**Can I embed `pkg/agent` in a binary that isn't mine?**
Yes — exactly what you just saw with the v1/v2/v3 apps. See the
"Embedding as a library" section of `README.md` for the full example.
