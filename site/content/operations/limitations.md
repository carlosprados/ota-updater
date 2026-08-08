---
title: "Known limitations"
weight: 51
description: "What this does not do, and why each gap was left open on purpose."
---

# Known limitations

Every item here was analysed and consciously deferred. Listing them is more
useful than discovering them in production.

## Hard ceilings

**`bsdiff` peaks at ~20× the binary size in RAM.** Targets above roughly
100 MiB are not practical on a modest server. `librsync` was benchmarked as a
substitute and rejected: on real Go binaries it produced deltas around 100×
larger, which defeats the purpose entirely. If you need to ship something that
big, the full-download path works — you just lose the delta advantage.

**`go-bsdiff` is not actively maintained.** Validate early against your real
binaries. `icedream/go-bsdiff` and `xdelta3` are the tracked alternatives.

## Deferred by design

### CoAP Block2 resume

An interrupted CoAP download restarts from block 0. HTTP with `Range` resumes
normally.

*Why deferred:* HTTP is the preferred transport for large transfers, where
resume actually matters; CoAP is acceptable for small deltas.
*Mitigation:* prefer `transport: http` when targets exceed ~100 KiB.

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

**`store.state_file` is effectively mandatory.** Without it, artifacts
published through the admin API vanish on restart.
