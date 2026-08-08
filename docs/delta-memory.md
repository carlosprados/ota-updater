# Delta generation memory: measurements and options

Authoritative record of where the server's memory goes during delta
generation, what the alternatives cost, and which were rejected and why.

Everything here was **measured, not estimated**. Reproduce with
`benchmark/delta-memory/` (see [Reproducing](#reproducing)).

Measured 2026-08-08 on linux/amd64, Go 1.26.1, using two consecutive real
builds of this project's own `update-server` as the artifact pair.

---

## 1. Where the memory goes

`bsdiff` peaks at roughly **21× the larger input**, and it scales linearly.

| Input | Peak RSS | Ratio | Generate |
|---|---:|---:|---:|
| 13.6 MB | 296 MiB | 21.7× | 3.2 s |
| 27.3 MB | 557 MiB | 20.4× | 24.9 s ⚠ |

⚠ The large-pair *time* is not representative: that pair is a duplicated file,
pathological for a suffix sort. The *memory* figure is representative, because
it depends on input size rather than content.

### Why 21×

`gabstv/go-bsdiff`, `pkg/bsdiff/bsdiff.go`:

```go
iii := make([]int, len(oldbin)+1)   // suffix array
vvv := make([]int, len(iii))        // inverse permutation
```

On 64-bit Go, `int` is 8 bytes. Those two arrays alone are **16 × N**. The
remaining ~5N is the old binary, the new binary, the patch buffer and GC
headroom.

The critical property: **the suffix array is computed, not file-backed.** The
kernel cannot reclaim it under pressure the way it reclaims the page cache. It
is anonymous memory, and on a host without swap it is the OOM killer's problem.

Extrapolation, per generation, multiplied by `delta_concurrency`:

| Artifact | Peak |
|---|---:|
| 22 MB | ~450 MiB |
| 50 MB | ~1.0 GiB |
| 100 MB | ~2.0 GiB |

---

## 2. What shipped: `delta_max_source_mb`

Implemented in v0.5.0. Default 32 MiB, **enabled**.

Above the cap the server does not diff; the manifester serves the whole
compressed target instead. This bounds the **absolute** cost. It does not
change the multiplier.

Design notes that are easy to get wrong on a second attempt:

- **The decision belongs to the manifester, not the store.** A store that
  merely refused would leave the manifester dispatching a generation, getting
  nothing, and answering `RetryAfter` on every heartbeat forever — the
  stranding failure the full-download path exists to remove.
- **The store enforces it too** (`ErrDeltaTooLarge`), because `pkg/server` is
  importable and a direct consumer must not be able to allocate the process to
  death.
- **A delta already on disk is always served**, whatever the cap says. The
  memory was spent when it was generated. Lowering the cap never invalidates
  work already done.
- **An absent binary is not a budget failure.** Reporting it as one sends
  callers down the wrong recovery path.

---

## 3. Option A — `int32` suffix array (measured, not adopted)

Change `iii` and `vvv` from `[]int` to `[]int32`. Valid for inputs below 2 GiB,
which is far beyond what bsdiff is practical for anyway.

| N | int64 (current) | int32 fork | Δ |
|---|---:|---:|---:|
| 13.6 MB | 296 MiB (21.7×) | **204 MiB (15.0×)** | −31% |
| 27.3 MB | 557 MiB (20.4×) | **409 MiB (15.0×)** | −27% |

Generation is also **~28% faster** on the large pair (24.9 s → 17.9 s): smaller
arrays, better cache locality.

**Verified by round-trip.** The stock `bspatch` applies the fork's patch and
reproduces the target byte for byte. The on-disk patch format is unchanged —
only the internal index width differs.

**Scope:** 510 lines total, and only 5 signatures touch `[]int`. The saved diff
is 66 changed lines: `benchmark/delta-memory/bsdiff-int32.patch`.

### Why it was not adopted (yet)

It means owning a fork of a diffing algorithm. Two honest counterweights:

- *For:* `go-bsdiff` is unmaintained, which this project's own docs already
  flag as a risk. Forking formalises exposure that exists either way. And the
  blast radius is contained: every reconstruction is verified against the
  signed `targetHash` before activation, so a bad patch fails closed and can
  never activate a wrong binary.
- *Against:* the quick conversion produces a patch **3% larger** (258,140 vs
  250,167 bytes). It is valid — it applies and reproduces — but **not
  bit-identical**, meaning the conversion changed a tie-break in the sort
  ordering. That difference must be understood before production, not shipped
  as-is.

**If picked up again, start there:** find why the ordering differs. Likely
candidates are the sentinel comparisons in `qsufsort` (`iii[0] != -(bufzise+1)`)
and the `int`/`int32` boundary inside `split`.

---

## 4. Option B — `zstd --patch-from` (measured, not adopted)

**A pure-Go implementation is viable with the dependency already present.**
`klauspost/compress` exposes `WithEncoderDictRaw` / `WithDecoderDictRaw`, and
its `MaxWindowSize` is 512 MiB — ample for a 34 MB reference. No CGO, no
external binary, no new dependency. The numbers below are from that pure-Go
path, not the zstd CLI.

| | Patch | Generate | Peak RSS | Apply | Apply RSS |
|---|---:|---:|---:|---:|---:|
| bsdiff + zstd | 250,167 B | 3.18 s | 296 MiB (21.7×) | 0.06 s | 62.9 MiB |
| zstd dict | 910,609 B | 0.27 s | **133 MiB (9.3×)** | 0.01 s | 45.7 MiB |

Recipe:

```go
enc, _ := zstd.NewWriter(nil,
    zstd.WithEncoderDictRaw(1, oldBin),
    zstd.WithWindowSize(nextPow2(len(oldBin))),   // must cover the reference
    zstd.WithEncoderLevel(zstd.SpeedBestCompression),
)
patch := enc.EncodeAll(newBin, nil)

dec, _ := zstd.NewReader(nil,
    zstd.WithDecoderDictRaw(1, oldBin),
    zstd.WithDecoderMaxWindow(1<<29),
)
out, _ := dec.DecodeAll(patch, nil)
```

### Why it should not replace bsdiff

**Generation is O(versions). Transfer is O(devices).**

A delta is generated once per `(from, to)` pair and cached; it is transferred
once per device. Optimising the term multiplied by the fleet beats optimising
the term amortised across versions.

The patch is **3.6× larger**: 250 KB → 910 KB. At 20 kbps that is **+4.4
minutes of radio per device**. For a thousand devices, 73 device-hours of extra
radio to save 160 MiB of peak on one server.

### Where it would make sense

As an **opt-in transfer mode per artifact**, for artifacts too large for bsdiff.
The docs currently call targets above ~100 MiB impractical; patch-from would
make them practical at 9.3× instead of 21×. Cost: a second transfer path to
sign, version in the protocol and test.

---

## 5. Rejected outright

**`icedream/go-bsdiff`** — uses **CGO** (`internal/native/cgo_write.go`). Breaks
`CGO_ENABLED=0`, which is a premise of this project for both the server and the
device agent. Not viable regardless of its merits.

**`librsync`** — benchmarked earlier (see `benchmark/`) and rejected: on real Go
binaries it produced deltas around **100× larger**, defeating the purpose.

---

## 6. The device side

`pkg/agent` reconstructs with `delta.Apply(oldBin, patch)`, which holds **four**
buffers simultaneously: the compressed patch, the decompressed patch, the base
binary, and the result. Measured peak: 62.9 MiB for a 13.6 MB artifact (~4.6×).

`mmap`-ing the base would remove one artifact-sized allocation (~22% of peak).
The stronger argument is qualitative rather than proportional: under a cgroup
limit, heap pages are **unreclaimable** while file-backed pages are
**reclaimable**, so mapping converts an OOM-kill into memory pressure.

Here the base is already a plain file — the active slot — so it can be mapped
directly. No temp file, no copy. Roughly 20 lines plus a `//go:build !linux`
fallback.

The result buffer cannot be avoided: `bspatch.Reader` and `bspatch.File` both
`ReadAll` into memory and delegate to the `[]byte` implementation, so
`go-bsdiff` has no streaming path at any entry point.

---

## Reproducing

```sh
# 1. Build a realistic pair: two consecutive builds of a real Go binary.
git worktree add --detach /tmp/pbwt v0.3.2
(cd /tmp/pbwt && CGO_ENABLED=0 go build -o /tmp/pb/v0 ./cmd/update-server)
CGO_ENABLED=0 go build -o /tmp/pb/v1 ./cmd/update-server

# 2. Run each mode in its own process; peak RSS is per-process.
# Build first: go run would fold the compiler into the measurement.
go build -o /tmp/pb/harness ./benchmark/delta-memory/harness
/usr/bin/time -f '%M KB' /tmp/pb/harness gen-bsdiff
/usr/bin/time -f '%M KB' /tmp/pb/harness gen-zstddict
```

`benchmark/delta-memory/harness/` implements the modes
(`gen-bsdiff`, `gen-zstddict`, `apply-bsdiff`, `apply-zstddict`) and verifies
every reconstruction against the target.

`harness-int32/` builds the forked bsdiff and asserts round-trip against the
**stock** `bspatch`, which is what makes the fork's patch trustworthy.

Two traps that cost time the first round:

- **Measure peak RSS per process.** `ru_maxrss` is a high-water mark; running
  several modes in one process reports the largest, not each.
- **Do not synthesise a large pair by concatenating a file with itself.** The
  memory figure stays valid but the timing becomes meaningless — duplicated
  content is pathological for a suffix sort.
