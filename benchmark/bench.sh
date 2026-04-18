#!/usr/bin/env bash
# Runs the bsdiff-vs-librsync matrix and prints a human table.
#
# Each combo runs in a fresh process so /proc/self/status VmHWM reflects that
# combo's peak alone.

set -euo pipefail

cd "$(dirname "$0")"
[[ -x ./deltabench ]] || go build -o deltabench .

header() {
    printf '\n%s\n' "$1"
    printf '%s\n' "------------------------------------------------------------------------------"
    printf '%-10s %-6s %-22s %12s %12s %14s %10s\n' \
        "algo" "size" "pattern/label" "delta-bytes" "delta-%base" "peak-rss" "wall-ms"
    printf '%s\n' "------------------------------------------------------------------------------"
}

format_line() {
    local algo size_bytes label seed delta_bytes peak_kb wall_ms
    IFS=$'\t' read -r algo size_bytes label seed delta_bytes peak_kb wall_ms <<< "$1"
    local size_mb pct rss_mb
    size_mb=$(awk -v b="$size_bytes" 'BEGIN{printf "%.1f", b/1048576}')
    pct=$(awk -v d="$delta_bytes" -v b="$size_bytes" 'BEGIN{printf "%.2f%%", 100*d/b}')
    rss_mb=$(awk -v k="$peak_kb" 'BEGIN{printf "%.2f MiB", k/1024}')
    printf '%-10s %5sM %-22s %12s %12s %14s %10s\n' \
        "$algo" "$size_mb" "$label" "$delta_bytes" "$pct" "$rss_mb" "$wall_ms"
}

run_one() {
    local line
    line=$(./deltabench "$@" 2>/dev/null)
    format_line "$line"
}

header "SYNTHETIC (structured pseudo-random: 70% template blocks + 30% unique)"
for size in 1 20 100; do
    for pat in small large; do
        for algo in bsdiff librsync; do
            run_one -algo "$algo" -size-mb "$size" -pattern "$pat"
        done
    done
done

header "REAL (two update-server Go binaries differing by one string literal)"
if [[ -f /tmp/bench_v1.bin && -f /tmp/bench_v2.bin ]]; then
    for algo in bsdiff librsync; do
        run_one -algo "$algo" -real-base /tmp/bench_v1.bin -real-target /tmp/bench_v2.bin
    done
else
    echo "  /tmp/bench_v1.bin and /tmp/bench_v2.bin not found — skipping real-binary row"
fi
