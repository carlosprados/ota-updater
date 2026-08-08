---
title: "Known limitations"
weight: 51
description: "What this does not do, and why each gap was left open on purpose."
---

Every item here was analysed and consciously deferred. Listing them is more
useful than discovering them in production.

## Hard ceilings

**`bsdiff` peaks at ~20× the binary size in RAM.** Targets above roughly
100 MiB are not practical on a modest server. `librsync` was benchmarked as a
substitute and rejected: on real Go binaries it produced deltas around 100×
larger, which defeats the purpose entirely. If you need to ship something that
big, the full-download path works — you just lose the delta advantage.

**`go-bsdiff` is not actively maintained.** Validate early against your real
binaries. `icedream/go-bsdiff`, `xdelta3` and **`zstd --patch-from`** are the
tracked alternatives.

`zstd --patch-from` has now been measured **in this repository**, against two
consecutive real builds of its own server binary, using a pure-Go
implementation (`klauspost/compress`, already a dependency — no CGO):

| | Patch | Generate | Peak RSS |
|---|---:|---:|---:|
| bsdiff + zstd | 250,167 B | 3.18 s | 296 MiB (21.7×) |
| zstd dictionary | 910,609 B | 0.27 s | 133 MiB (9.3×) |

A third of the memory for **3.6× the patch**. It was rejected as a replacement
because generation is O(versions) and amortises across the fleet, while
transfer is O(devices) and multiplies: 660 KB more per device is over four
extra minutes of radio at 20 kbps. It remains the strongest candidate as an
opt-in mode for artifacts too large to diff.

An `int32` suffix array was also measured, reaching 15.0× instead of 21.7× and
running 28% faster, verified by round-trip against the stock patcher.

Full methodology, numbers and the saved patch:
[`docs/delta-memory.md`](https://github.com/carlosprados/ota-updater/blob/main/docs/delta-memory.md).

## Deferred by design

### CoAP Block2 resume

An interrupted CoAP download restarts from block 0. HTTP with `Range` resumes
normally.

*Why deferred:* HTTP is the preferred transport for large transfers, where
resume actually matters; CoAP is acceptable for small deltas.
*Mitigation:* prefer `transport: http` when targets exceed ~100 KiB.

**This is the top of the CoAP backlog, above DTLS.** The CoAP path exists for
links too poor for HTTPS — and those are exactly the links where a transfer
gets interrupted. A 1 MiB delta at 20–60 kbps is two to seven minutes of radio
that any interruption throws away in full. Confidentiality of an
already-signed binary is a weaker requirement than finishing the download at
all.

### Device-side memory during a CoAP transfer

The CoAP path buffers the entire payload in memory (`resp.ReadBody()`), and
`bsdiff`'s `[]byte` API then holds the old binary, the new binary and the
patch simultaneously. Peak device-side RSS is therefore roughly

    old + new + patch + transfer buffer

which for a 2 MiB binary with a 200 KiB patch lands near 6 MiB on top of the
process baseline. Size `GOMEMLIMIT` and the cgroup limit accordingly — see
[Operations]({{% relref "/operations" %}}). This is the device-side analogue
of the server's ~20× `bsdiff` ceiling.

### Per-IP admin rate limiting

The bucket is global, so a distributed attack from many IPs saturates the same
bucket.

*Why deferred:* no evidence of distributed attack in practice, and firewalling
the admin port to known sources covers the realistic case.
*Retake it if:* you see 429s in the logs that do **not** all come from one IP.

### Server-side clock-skew validation

`Heartbeat.Timestamp` is logged but not validated against the server clock.

*Why deferred:* the field is advisory — the real cryptographic gate is the
Ed25519 manifest signature, and the heartbeat carries nothing signed that an
attacker could usefully freeze. Judging thresholds without real fleet data
would be guesswork.
*If implemented:* warn plus a histogram metric, **never** enforcement.
Blocking on skew would reject devices with a broken RTC, which are precisely
the ones most in need of an update.

### Replay of old signed manifests

An attacker who captures a valid manifest can replay it later; the agent will
apply an old-but-validly-signed transfer. Mitigation would be a monotonic
counter or timestamp inside the signed payload.

*Why deferred:* the attack downgrades to a previously-authorised version, not
to arbitrary code, and requires an active MitM. Worth fixing if you ship a
security patch that must not be rolled back.

## Not implemented at all

- **No DTLS.** CoAP is plain UDP on 5683. `coaps://` with PSK is a future
  extension. The manifest signature is what provides integrity; confidentiality
  of the binary is not currently a goal.
- **No binary upload endpoint.** `POST /admin/artifacts` takes a *path on the
  server's filesystem*, not a payload. Publishing still requires getting bytes
  onto the host by other means (scp, shared volume, object-store sync).
- **No multi-tenancy.** One key, one admin token, one flat artifact namespace.

## Operational sharp edges

**Docker without a persistent volume loses updates.** The A/B slots must
survive container recreation, or every update evaporates on the next deploy.

**`ExecStart=` must point at the symlink.** Pointing it at a slot file means
systemd restarts into the *old* binary after a swap.

**Retention off means unbounded disk.** It is off by default for safety, which
means the default configuration grows forever. Turn it on before you need it.

**Retention silently converts deltas into full downloads.** Sweeping a binary
that devices still run does not break them — the full-download path catches it
— but every device on that version switches from a small patch to the whole
compressed target. On NB-IoT that is the difference between a minute and a
quarter of an hour. `history_depth` is the knob that decides how far behind a
device may fall before it pays that cost, and
`updater_deltas_served_total{mode="full"}` is how you notice it happening.

**Hot-cache sizing changed meaning with multi-artifact.** `hot_delta_cache_mb`
used to be sized against one target's working set; it is now shared across
every artifact being served concurrently. An artifact whose transfers exceed
the whole budget is never cached at all — the byte-budget LRU rejects values
larger than itself — so every request re-reads it from disk, including each
Range request from a resuming device. Size it against the sum of the artifacts
you expect to be mid-campaign at once, not against the largest one.

**`store.state_file` is effectively mandatory.** Without it, artifacts
published through the admin API vanish on restart.
