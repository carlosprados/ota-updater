---
title: "Protocol"
weight: 20
description: "Wire messages, the two transfer modes, and the exact sequence of an update cycle."
---

# Protocol

## Resource paths

HTTP and CoAP mirror each other exactly — both handlers read their paths from
the same constants in `pkg/protocol`, so they cannot drift.

| Path | Method | HTTP body | CoAP body |
|---|---|---|---|
| `/heartbeat` | POST | JSON `Heartbeat` → `ManifestResponse` | CBOR |
| `/delta/{from}/{to}` | GET | compressed patch, Range-capable | Block2 |
| `/binary/{hash}` | GET | whole compressed target, Range-capable | Block2 |
| `/report` | POST | JSON `UpdateReport` | CBOR |
| `/health` | GET | JSON status + artifact map | — |

Admin endpoints (`/admin/*`) are HTTP-only and Bearer-authenticated.

## Messages

One Go type per message, carrying **both** JSON and CBOR tags. CBOR uses
integer keys (`cbor:"N,keyasint"`) rather than strings, which matters on
NB-IoT: field names would otherwise be a meaningful fraction of a heartbeat.

```mermaid
classDiagram
    class Heartbeat {
        +string DeviceID
        +string VersionHash
        +HWInfo HWInfo
        +int64 Timestamp
        +string Version
        +string Artifact
    }

    class HWInfo {
        +string Arch
        +string OS
        +uint64 FreeRAM
        +uint64 FreeDisk
    }

    class ManifestResponse {
        +bool UpdateAvailable
        +string TargetVersion
        +string TargetHash
        +int64 TargetSize
        +int64 DeltaSize
        +string DeltaHash
        +int ChunkSize
        +int TotalChunks
        +string Signature
        +string DeltaEndpoint
        +string BinaryEndpoint
        +int RetryAfter
        +string Artifact
    }

    class UpdateReport {
        +string DeviceID
        +string PreviousHash
        +string NewHash
        +bool Success
        +string RollbackReason
        +int64 Timestamp
    }

    Heartbeat *-- HWInfo
    Heartbeat ..> ManifestResponse : answered by
```

Two fields deserve explanation:

- **`Heartbeat.Artifact`** names the publication track, as
  `"name/os/arch"` or just `"name"`. **Empty means "the server's default
  artifact"**, which is what keeps single-artifact deployments — and agents
  built before multi-artifact support existed — working with no change.
- **`ManifestResponse.DeltaHash`** is the hash of *the bytes that travel on
  the wire*, whichever mode is in play. The name predates the full-download
  mode; the semantics are "transfer hash".

## Two transfer modes

Exactly one endpoint field is set on an actionable response, and it selects
how the agent reconstructs the target.

```mermaid
flowchart TD
    HB["heartbeat arrives<br/><i>version_hash = V</i>"] --> RESOLVE{"resolve artifact"}
    RESOLVE -->|unknown| E404["404 Not Found<br/><i>client mistake, not an outage</i>"]
    RESOLVE -->|ok| SAME{"V == target hash?"}

    SAME -->|yes| NOUP["UpdateAvailable = false"]
    SAME -->|no| KNOWN{"does the store<br/>hold binary V?"}

    KNOWN -->|yes| CACHED{"delta V to target<br/>cached?"}
    CACHED -->|no| RETRY["UpdateAvailable = true<br/>RetryAfter > 0<br/><i>async bsdiff dispatched</i><br/><b>no signature</b>"]
    CACHED -->|yes| DELTAM["<b>delta mode</b><br/>delta_endpoint set<br/>signed"]

    KNOWN -->|no| ALLOW{"allow_full_download?"}
    ALLOW -->|yes| FULLM["<b>full mode</b><br/>binary_endpoint set<br/>signed"]
    ALLOW -->|no| STRAND["UpdateAvailable = false<br/><i>device stays stranded</i>"]

    classDef ok fill:#e6f4ea,stroke:#34a853
    classDef warn fill:#fef7e0,stroke:#f9ab00
    classDef bad fill:#fce8e6,stroke:#d93025
    class DELTAM,FULLM ok
    class RETRY,NOUP warn
    class E404,STRAND bad
```

The right-hand branch is the one that did not exist originally. A device whose
current binary the server has never seen — factory-flashed, sideloaded, or a
version whose source binary aged out of retention — used to receive
`UpdateAvailable=false` on **every** heartbeat, forever. That is a silent,
permanent strand that looks like a healthy fleet in the logs.

See [Full-download fallback]({{% relref "/protocol/full-download" %}}) for why
the signature scheme did not have to change to accommodate it.

## A complete update cycle

```mermaid
sequenceDiagram
    autonumber
    participant A as edge-agent
    participant S as update-server
    participant D as disk (A/B slots)

    Note over A: running version V<br/>in slot A

    A->>S: POST /heartbeat {device_id, V, artifact}
    S->>S: resolve artifact → target T
    S->>S: delta V→T not cached
    S-->>A: UpdateAvailable, RetryAfter=30<br/>(no signature)
    Note over S: async bsdiff runs,<br/>bounded by delta_concurrency

    A->>S: POST /heartbeat (next cycle)
    S-->>A: signed manifest<br/>{T, delta_hash, signature, delta_endpoint}

    rect rgb(232, 240, 254)
        Note over A: verify signature BEFORE downloading
        A->>A: Ed25519.Verify(pub, T ‖ delta_hash, sig)
    end

    A->>S: GET /delta/V/T  (Range-resumable)
    S-->>A: compressed patch bytes

    rect rgb(232, 240, 254)
        Note over A: verify transfer BEFORE patching
        A->>A: SHA256(bytes) == delta_hash?
    end

    A->>D: bspatch(slot A, patch) → slot B
    A->>A: SHA256(slot B) == T?
    A->>D: write .pending_update
    A->>D: atomic symlink swap → slot B
    A->>A: syscall.Exec(new binary)

    Note over A: new binary boots

    A->>A: BootPhase: read .pending_update
    A->>S: heartbeat (health check, up to 3 tries)
    S-->>A: 200
    A->>D: clear .pending_update, reset boot count
    A->>S: POST /report {V→T, success=true}
```

### Why the verification order is not negotiable

Three checks, each catching something the others cannot:

| Step | Catches | Cost if it fires |
|---|---|---|
| Verify signature **before** download | A tampered or forged manifest | Zero bytes transferred |
| Verify transfer hash **before** patching | A corrupt or substituted payload | Bytes wasted, but no CPU — **the NB-IoT save** |
| Verify reconstruction **before** swap | Local disk corruption, bspatch bugs, a flipped transfer mode | CPU wasted, but no bad binary activated |

The middle one is the reason the signature covers the transfer hash at all.
On a 20 kbps link a 2 MiB payload is thirteen minutes of radio time; finding
out it was corrupt *after* also spending the CPU and RAM of a `bspatch` would
be strictly worse.

## Failure and retry

```mermaid
sequenceDiagram
    autonumber
    participant A as edge-agent
    participant P as primary transport
    participant F as fallback transport

    A->>P: heartbeat
    P--xA: transport error
    Note over A: one-shot fallback,<br/>NOT sticky
    A->>F: heartbeat
    F-->>A: manifest

    Note over A,F: next cycle starts over at the primary

    A->>P: heartbeat
    P-->>A: manifest
```

The fallback is deliberately **not sticky**: a cycle that failed over to CoAP
returns to HTTP on the next tick. A transient failure should not permanently
demote the preferred transport, and a persistent one costs only one extra
attempt per cycle to rediscover.

Downloads retry with exponential backoff and jitter, resuming via HTTP `Range`
from the last verified offset. Two special cases short-circuit the normal
backoff:

- **Disk full** (`ENOSPC`) applies a 5-minute floor. Burning retries against a
  static condition helps nobody.
- **A failed restart** arms a persisted cooldown (default 1h), because a
  restart that fails is almost always structural rather than transient.
