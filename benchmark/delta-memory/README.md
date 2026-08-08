# delta-memory

Harnesses behind [`docs/delta-memory.md`](../../docs/delta-memory.md), which is
the document to read first — it has the numbers and the conclusions. This
directory only exists so they can be reproduced without redoing the setup.

| File | What it is |
|---|---|
| `harness/` | Modes `gen-bsdiff`, `gen-zstddict`, `apply-bsdiff`, `apply-zstddict`. Every mode verifies its reconstruction against the target. |
| `harness-int32/` | Builds the forked bsdiff and applies its patch with the **stock** `bspatch`. That cross-check is what makes the fork's output trustworthy. |
| `bsdiff-int32.patch` | The `[]int` → `[]int32` conversion of `gabstv/go-bsdiff@v1.0.5` `pkg/bsdiff/bsdiff.go`. 66 changed lines. Measured at 15.0× peak versus 21.7×. |

These are not wired into `task` or CI: they need a realistic multi-megabyte
artifact pair, which is built ad hoc rather than committed.

## Running

```sh
# A realistic pair: two consecutive builds of a real Go binary.
mkdir -p /tmp/pb
git worktree add --detach /tmp/pbwt v0.3.2
(cd /tmp/pbwt && CGO_ENABLED=0 go build -o /tmp/pb/v0 ./cmd/update-server)
CGO_ENABLED=0 go build -o /tmp/pb/v1 ./cmd/update-server
git worktree remove /tmp/pbwt

# Each mode in its own process: ru_maxrss is a high-water mark, so running
# several in one process reports the largest rather than each.
# Build first: `go run` would fold the compiler into the measurement.
go build -o /tmp/pb/harness ./harness
/usr/bin/time -f '%M KB' /tmp/pb/harness gen-bsdiff
```

To rebuild the fork:

```sh
cp "$(go env GOMODCACHE)/github.com/gabstv/go-bsdiff@v1.0.5/pkg/bsdiff/bsdiff.go" .
chmod +w bsdiff.go
patch bsdiff.go < bsdiff-int32.patch
```

`bsdiff.go` carries Colin Percival's BSD-2-Clause notice; the patch is a
derivative and that notice must travel with it if the fork is ever adopted.

## If you pick the fork up again

Start with the open question, not the code: the conversion produces a patch
**3% larger** than the original (258,140 vs 250,167 bytes). It is valid — it
applies and reproduces the target byte for byte — but not bit-identical, so
some tie-break in the sort ordering changed. Likely candidates are the sentinel
comparisons in `qsufsort` and the `int`/`int32` boundary inside `split`.

Shipping it without understanding that difference would be adopting a silent
behavioural change in a diffing algorithm, which is exactly the class of thing
that is expensive to debug later.
