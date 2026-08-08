---
title: "Architecture"
weight: 10
description: "Components, the content-addressed store, and why the split between bytes and bookkeeping matters."
---

## Package layout

Everything under `pkg/` is exported and importable. There is no `internal/`
directory: the server is a library that also ships a binary, not a binary with
some code hidden inside it.

```mermaid
flowchart TB
    subgraph CMD["cmd/"]
        EA["edge-agent<br/><i>~190 line wrapper</i>"]
        US["update-server"]
    end

    subgraph PKG["pkg/ — exported"]
        AGENT["agent<br/><i>Updater, SlotManager,<br/>Watchdog, Downloader</i>"]
        SERVER["server<br/><i>Store, Registry, Manifester,<br/>handlers, Retention</i>"]
        PROTO["protocol<br/><i>wire types, ArtifactKey</i>"]
        CRYPTO["crypto<br/><i>Ed25519 + PEM</i>"]
        DELTA["delta<br/><i>bsdiff + zstd</i>"]
        COMP["compression<br/><i>zstd</i>"]
        AIO["atomicio<br/><i>durable writes</i>"]
    end

    EA --> AGENT
    US --> SERVER
    AGENT --> PROTO & CRYPTO & DELTA & COMP & AIO
    SERVER --> PROTO & CRYPTO & DELTA & COMP & AIO
    DELTA --> COMP

    classDef entry fill:#f9ab002e,stroke:#f9ab00
    classDef lib fill:#4285f42e,stroke:#4285f4
    class EA,US entry
    class AGENT,SERVER,PROTO,CRYPTO,DELTA,COMP,AIO lib
```

`protocol` is the only package both sides depend on for wire compatibility.
Its structs carry **dual JSON and CBOR tags**, so one type serializes over
HTTP (JSON) and CoAP (CBOR) with no duplication and no risk of the two
representations drifting apart.

## The store is content-addressed

This is the load-bearing decision. Nothing in the store is keyed by version,
name, or artifact — only by SHA-256 of content.

```mermaid
flowchart LR
    subgraph DISK["on disk"]
        direction TB
        B1["binaries/<br/><b>{sha256}.bin</b>"]
        D1["deltas/<br/><b>{from}_{to}.delta.zst</b><br/><b>{hash}.full.zst</b>"]
    end

    subgraph RAM["in RAM — byte-budgeted LRUs"]
        direction TB
        TC["target cache<br/><i>uncompressed targets,<br/>shared by all artifacts</i>"]
        HC["hot cache<br/><i>transfer bytes:<br/>deltas + full binaries</i>"]
    end

    subgraph NEVER["never in RAM"]
        SRC["source binaries<br/><i>kernel page cache<br/>does the LRU</i>"]
    end

    B1 --> TC
    D1 --> HC
    B1 -.-> SRC

    classDef disk fill:#5f63682e,stroke:#5f6368
    classDef ram fill:#34a8532e,stroke:#34a853
    classDef never fill:#d930252e,stroke:#d93025
    class B1,D1 disk
    class TC,HC ram
    class SRC never
```

Three consequences follow directly:

1. **Two artifacts shipping identical bytes share one file.** Publishing the
   same build on `agent/linux/arm64` and `agent/linux/amd64` costs one
   `.bin`, not two.
2. **A delta is identified by its `(from, to)` pair alone**, regardless of
   which artifact asked for it. The delta endpoint therefore needed no
   artifact segment when multi-artifact support was added.
3. **Deduplication is free and automatic.** Re-publishing an unchanged build
   is a no-op: same hash, same file, no cache invalidation, and no device
   sees a spurious update.

## Bytes vs bookkeeping

```mermaid
classDiagram
    class Store {
        <<content-addressed>>
        +RegisterBinary(data) hash
        +HasBinary(hash) bool
        +LoadBinary(hash) bytes
        +EnsureDelta(from, to) path
        +GetDeltaBytes(from, to) bytes
        +GetBinaryBytes(hash) bytes
    }
    note for Store "Knows nothing about versions,\nnames or artifacts. Addressed\npurely by SHA-256."

    class Registry {
        <<publication state>>
        +PublishBytes(key, version, data)
        +PublishFile(key, version, path)
        +Resolve(name) Artifact
        +LiveHashes() set
        +CurrentTargets() set
    }

    class Artifact {
        +ArtifactKey Key
        +string Version
        +string TargetHash
        +int64 TargetSize
        +string Source
        +[]string History
    }

    class ArtifactKey {
        +string Name
        +string OS
        +string Arch
        +String() "name/os/arch"
    }

    Registry "1" o-- "N" Artifact
    Artifact *-- ArtifactKey
    Registry ..> Store : publishes bytes into
```

The `Registry` is the only component that knows what "current" means. It
persists to a JSON state file, because a server that forgets which version is
current after a restart is not a source of truth for a fleet — it is an
outage.

`History` exists for retention: a superseded target is still a plausible delta
source for a device that has not checked in yet, so the sweeper must not
collect it.

## Request path for a transfer

Concurrent requests for the same uncached artifact collapse through
`singleflight`, so a campaign burst — thousands of devices asking for the same
delta within seconds of each other — becomes **one** disk read or **one**
bsdiff run, not thousands.

```mermaid
flowchart TD
    REQ["GET /delta/{from}/{to}"] --> VALID{"both segments<br/>valid SHA-256 hex?"}
    VALID -->|no| NF["404"]
    VALID -->|yes| CUR{"is {to} a current<br/>target of some artifact?"}
    CUR -->|no| NF
    CUR -->|yes| HOT{"in hot cache?"}

    HOT -->|hit| SERVE["serve from RAM<br/><i>zero I/O</i>"]
    HOT -->|miss| DISK{"on disk?"}

    DISK -->|yes| SF["singleflight:<br/>read file ONCE"]
    SF --> POP["populate hot cache"] --> SERVE

    DISK -->|no| GEN["dispatch async bsdiff<br/><i>bounded by delta_concurrency</i>"]
    GEN --> R404["404 → agent retries<br/>after RetryAfter"]

    classDef good fill:#34a8532e,stroke:#34a853
    classDef bad fill:#d930252e,stroke:#d93025
    class SERVE good
    class NF,R404 bad
```

The "is `{to}` a current target" check is not cosmetic. Without it, anyone able
to name two known hashes could ask an unauthenticated endpoint to run an
arbitrary `bsdiff` — the most expensive operation the process performs, at
roughly 20× the binary size in peak RAM. Restricting the destination bounds
what a request can make the server do.

## Durability

Every write that must survive power loss goes through `pkg/atomicio`, which
guarantees the same three steps in order:

```mermaid
flowchart LR
    W["write to<br/>.tmp-XXXX"] --> S["f.Sync()<br/><i>content durable</i>"]
    S --> R["rename()<br/><i>atomic swap</i>"]
    R --> D["fsync(parent dir)<br/><i>dirent durable</i>"]

    classDef step fill:#4285f42e,stroke:#4285f4
    class W,S,R,D step
```

The final `fsync` of the parent directory is the step most implementations
skip. Without it, a rename can be lost across a power cut even though the file
contents were synced — the classic "the file is there but empty, or not there
at all" failure on unclean shutdown.

Callers unified behind it: the store's binaries and deltas, the agent's slot
writes and symlink swap, the pending-update marker, the boot counter, the
download resume state, and the registry state file.

A crash between `create` and `rename` leaves a `.tmp-*` file behind; both the
store and the agent sweep stragglers older than 24h at startup.
