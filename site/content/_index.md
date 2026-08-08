---
title: "OTA Updater"
weight: 1
---

# OTA Updater

A Go library and server for **over-the-air updates on constrained networks** —
built for NB-IoT links at 20–60 kbps with high latency and frequent
disconnects, where a naive "download the new binary" strategy is not viable.

```sh
go get github.com/carlosprados/ota-updater
```

Apache-2.0. Both halves are importable Go packages, not just binaries.

## What it actually gives you

| | |
|---|---|
| **Binary deltas** | `bsdiff` + `zstd`. Ship a patch instead of the whole binary — usually one to two orders of magnitude less downlink. |
| **Signed everything** | Ed25519 over `(target hash, transferred bytes hash)`. The agent rejects a corrupt payload *after downloading it but before patching*, which on NB-IoT is the save that matters. |
| **A/B slots + rollback** | The new binary is staged in the inactive slot. A watchdog confirms health after the swap; a version that fails to come up twice is marked bad permanently. |
| **Multi-artifact** | One server, N components × M versions × K architectures. Content-addressed storage means two tracks shipping identical bytes share one file. |
| **Never strands a device** | If the server cannot build a patch — factory-flashed unit, sideloaded build, corrupt slot — it ships the whole compressed target instead. |
| **Bounded resources** | RAM is capped by explicit byte budgets regardless of history size or artifact count. Disk is reclaimed by a retention sweeper. |
| **Two transports** | HTTP/JSON and CoAP/CBOR, mirroring the same resource paths. The agent falls back between them once per cycle. |

## The shape of the system

```mermaid
flowchart LR
    subgraph OPS["Operator / CI"]
        BUILD["build artifact"]
    end

    subgraph SERVER["update-server"]
        REG["Registry<br/><i>which hash is current<br/>per artifact</i>"]
        STORE["Store<br/><i>content-addressed:<br/>hash.bin, from_to.delta.zst</i>"]
        MAN["Manifester<br/><i>signs manifests</i>"]
        RET["Retention<br/><i>reclaims disk</i>"]
    end

    subgraph DEVICE["Device (edge-agent)"]
        UPD["Updater"]
        SLOTS["A/B slots<br/>+ watchdog"]
    end

    BUILD -->|"file drop or<br/>POST /admin/artifacts"| REG
    REG -->|"target hash"| MAN
    STORE <-->|"bytes"| MAN
    REG -.->|"live set"| RET
    RET -.->|"deletes stale"| STORE

    UPD -->|"1 . POST /heartbeat<br/><i>device_id, version_hash, artifact</i>"| MAN
    MAN -->|"2 . signed manifest"| UPD
    UPD -->|"3 . GET /delta/from/to<br/>or /binary/hash"| STORE
    STORE -->|"4 . compressed bytes"| UPD
    UPD --> SLOTS
    SLOTS -->|"5 . POST /report"| SERVER

    classDef srv fill:#e8f0fe,stroke:#4285f4,stroke-width:1px
    classDef dev fill:#e6f4ea,stroke:#34a853,stroke-width:1px
    class REG,STORE,MAN,RET srv
    class UPD,SLOTS dev
```

The division of labour is worth stating plainly, because it is the design
decision everything else follows from:

- The **Store** knows bytes and nothing else. It is addressed purely by
  SHA-256 and has no concept of "current", "version", or "artifact".
- The **Registry** knows which bytes are current for which publication track.
  It is the *only* per-artifact state in the system.

That split is why one server handles N components × M versions ×
K architectures without duplicating storage, and why adding an artifact costs
bookkeeping rather than disk.

## Where to go next

{{% children description="true" %}}

## Status

Feature-complete: both binaries build static (`CGO_ENABLED=0`), the unit suite
runs green under `-race`, and an end-to-end integration test drives a real
agent against a real in-process server through a full update cycle.

Known gaps are documented honestly in
[Known limitations]({{% relref "/operations/limitations" %}}) — including the
ones deliberately deferred.
