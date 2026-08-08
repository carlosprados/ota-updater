---
title: "Signature scheme"
weight: 21
description: "What is signed, in what order it is verified, and the threat model."
---

{{% notice style="info" title="Authoritative source" %}}
`docs/signing.md` in the repository is the normative document. This page is a
readable companion; if the two ever disagree, the repository wins.
{{% /notice %}}

## What is signed

```
payload   = target_hash_raw ‖ transfer_hash_raw     // 32 + 32 = 64 bytes
signature = Ed25519.Sign(server_private_key, payload)
```

- **`target_hash_raw`** — raw SHA-256 of the *reconstructed target binary*,
  i.e. exactly what will run on the device afterwards.
- **`transfer_hash_raw`** — raw SHA-256 of *the bytes that travel on the
  wire*. No other transformation.

Strict byte-wise concatenation, target first. **No separators, no length
prefix, no version byte** — both halves are the same algorithm at the same
fixed length, so there is no ambiguity to resolve.

The payload is **not** re-hashed before signing: Ed25519 hashes the message
internally with SHA-512, so an outer SHA-256 would be a redundant layer that
only adds a place for implementations to disagree.

## One scheme, two modes

This is the part worth understanding, because it is why adding whole-binary
downloads required no cryptographic change at all.

```mermaid
flowchart LR
    subgraph DELTA["delta mode"]
        direction TB
        DT["target binary"] --> DTH["target_hash"]
        DP["zstd(bsdiff patch)"] --> DPH["transfer_hash"]
    end

    subgraph FULL["full mode"]
        direction TB
        FT["target binary"] --> FTH["target_hash"]
        FP["zstd(target binary)"] --> FPH["transfer_hash"]
    end

    DTH & DPH --> SIG["<b>same 64-byte payload</b><br/>Ed25519.Sign"]
    FTH & FPH --> SIG

    classDef mode fill:#4285f42e,stroke:#4285f4
    classDef sig fill:#34a8532e,stroke:#34a853
    class DT,DTH,DP,DPH,FT,FTH,FP,FPH mode
    class SIG sig
```

Because `transfer_hash` was defined from the start as "the hash of whatever
bytes are transferred" rather than "the hash of the patch", a full download is
formally just *a delta from nothing*.

{{% notice style="note" title="Why the full binary is served compressed" %}}
It is not only a bandwidth choice. If the target were served raw, then
`transfer_hash == target_hash` and the signed payload would degenerate to
`H ‖ H` — a distinguishable, weaker special case that every implementation
would then have to handle separately. Compressing keeps the two halves
independent and the scheme uniform.
{{% /notice %}}

## Verification order on the agent

```mermaid
stateDiagram-v2
    [*] --> Received: manifest with<br/>UpdateAvailable=true

    Received --> VerifySig: rebuild canonical payload
    VerifySig --> Abort: signature invalid
    VerifySig --> PickMode: signature valid

    PickMode --> Abort: neither endpoint set
    PickMode --> Download: binary_endpoint → full<br/>delta_endpoint → delta

    Download --> CheckTransfer: bytes on disk
    CheckTransfer --> Abort: SHA256 ≠ delta_hash
    CheckTransfer --> Reconstruct: matches

    Reconstruct --> CheckTarget: bspatch (delta)<br/>or decompress (full)
    CheckTarget --> Abort: SHA256 ≠ target_hash
    CheckTarget --> Stage: matches

    Stage --> Swap: write inactive slot<br/>+ .pending_update
    Swap --> [*]: symlink swap, exec

    Abort --> [*]: no swap, device<br/>keeps running old version

    note right of VerifySig
        Before any download.
        Costs zero bytes.
    end note

    note right of CheckTransfer
        Before any patching.
        The NB-IoT save.
    end note
```

Every abort path leaves the device running its current binary. There is no
state in which a failed update degrades what was already working.

## Threat model

| Attack | Outcome |
|---|---|
| MitM forges or tampers with the manifest | Rejected at signature verification. Nothing downloaded. |
| MitM tampers with the transferred payload | Rejected at the transfer-hash check, before any CPU is spent patching. |
| MitM flips the transfer mode (swaps which endpoint field is set) | The mode is **not** signed — but the flip cannot produce a valid binary. The agent either bspatches a compressed binary or treats a patch as one; the result fails the authenticated `target_hash` check and the swap never happens. Cost: one wasted download. |
| MitM replays an older signed manifest | **Possible.** The agent will apply an old-but-validly-signed transfer. Mitigation, if it becomes a concern, is a monotonic counter in the signed payload. Documented rather than hidden. |
| Compromise of `server.key` | Total. The attacker can sign anything for any target. Mitigation is operational: isolated host, offline backup, no unrelated services on the box. |

## Keys

| | Format | Mode | Lives on |
|---|---|---|---|
| `server.key` | PKCS#8 DER in PEM | `0600` | Update server only. Never leaves it. |
| `agent.pub` | PKIX DER in PEM | `0644` | Shipped with the agent binary or embedded in firmware. |

```sh
go run ./tools/keygen -out ./keys
```

`keygen` refuses to overwrite existing files (`O_EXCL`). Destroying a keypair
has to be a deliberate, manual act — losing `server.key` means you can no
longer issue updates to any device that already trusts the matching public
key.

## Operator workflow

Signing is **not** a release step. There is no command to run, no artifact to
sign offline, no key to feed into CI.

```mermaid
sequenceDiagram
    participant CI
    participant FS as server filesystem
    participant S as update-server
    participant A as agents

    Note over CI,S: once, at setup
    CI->>S: keygen → server.key stays, agent.pub ships

    Note over CI,A: every release
    CI->>FS: cp new-binary /srv/artifacts/agent-arm64
    FS-->>S: fsnotify event
    S->>S: hash, register, publish, invalidate caches
    A->>S: heartbeat
    S->>S: sign manifest on the fly<br/><i>~50 µs, in-memory key</i>
    S-->>A: signed manifest
```

Signatures are never cached on disk. Ed25519 signing is sub-millisecond, and
always rebuilding removes a whole class of staleness bugs — a signature
cached against a stale transfer hash is exactly the kind of defect that
survives testing and surfaces in the field.
