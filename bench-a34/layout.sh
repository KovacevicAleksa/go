#!/bin/bash
# Checks whether the goroutine creation differences between the chain
# variants come from the change itself or from where the linker places
# functions. Builds each variant's runtime test binary once with the
# default layout (seed 0) and once with -ldflags=-randlayout=N for
# N = 1..SEEDS, then runs every binary once per round, rotating the
# variant order every round.
#
# If the variants differ by about as much as one variant differs from
# itself across layouts, the difference measures code placement, not the
# change.
#
# Usage: layout.sh <work dir> [rounds]
# Env: BASE (commit the diffs apply to), GOSRC (a local Go checkout of
# BASE to use instead of fetching), VARIANTS, SEEDS (default 7), CPUS.
# Output: <work dir>/layout/, summary in layout-summary.txt.
set -euo pipefail

BASE=${BASE:-dfe2e7bfb454f71784ee1aeeac44063fd582f9f3}
VARIANTS=${VARIANTS:-master chain-base chain-a5a chain-final chain-ctl}
SEEDS=${SEEDS:-7}
HERE=$(cd "$(dirname "$0")" && pwd)
W=$(mkdir -p "$1" && cd "$1" && pwd)
ROUNDS=${2:-4}
R=$W/layout
mkdir -p "$R" "$W/bin"
NCPU=$(getconf _NPROCESSORS_ONLN)
CPUS=${CPUS:-1,2,$NCPU}
BENCH='^Benchmark(CreateGoroutines|CreateGoroutinesParallel|CreateGoroutinesSingle|CreateGoroutinesCapture|StackGrowth)$'

{
	date -u
	uname -a
	lscpu 2>/dev/null || sysctl -n machdep.cpu.brand_string hw.cachelinesize 2>/dev/null || true
	echo "variants: $VARIANTS; seeds 0..$SEEDS; rounds $ROUNDS; cpus $CPUS"
} > "$R/machine.txt"

if [ -n "${GOSRC:-}" ]; then
	G=$GOSRC
else
	G=$W/go
	if [ ! -e "$G/.git" ]; then
		git init -q "$G"
		git -C "$G" fetch -q --depth 1 https://github.com/golang/go "$BASE"
		git -C "$G" checkout -q FETCH_HEAD
	fi
fi
test "$(git -C "$G" rev-parse HEAD)" = "$BASE"
if [ ! -x "$G/bin/go" ]; then
	(cd "$G/src" && ./make.bash)
fi
GO=$G/bin/go
"$GO" version | tee "$R/go-version.txt"

reset_tree() {
	git -C "$G" checkout -q -- src
	git -C "$G" clean -fdq src
}

seeds=(0)
for ((s = 1; s <= SEEDS; s++)); do
	seeds+=("$s")
done

for v in $VARIANTS; do
	reset_tree
	if [ "$v" != master ]; then
		git -C "$G" apply "$HERE/$v.diff"
	fi
	for s in "${seeds[@]}"; do
		ld=
		if [ "$s" != 0 ]; then
			ld=-randlayout=$s
		fi
		"$GO" test -c -ldflags="$ld" -o "$W/bin/$v-$s.test" runtime
	done
done
reset_tree

read -r -a vs <<< "$VARIANTS"
n=${#vs[@]}
for ((i = 0; i < ROUNDS; i++)); do
	order=()
	for ((j = 0; j < n; j++)); do
		order+=("${vs[(i + j) % n]}")
	done
	echo "round $((i + 1))/$ROUNDS $(date +%H:%M:%S): ${order[*]}"
	for v in "${order[@]}"; do
		for s in "${seeds[@]}"; do
			"$W/bin/$v-$s.test" -test.run='^$' -test.bench="$BENCH" \
				-test.cpu="$CPUS" >> "$R/$v-$s.txt"
		done
	done
done

# Every sample as "benchmark variant seed ns/op".
for v in $VARIANTS; do
	for s in "${seeds[@]}"; do
		awk -v v="$v" -v s="$s" '/^Benchmark/ { print $1, v, s, $3 }' "$R/$v-$s.txt"
	done
done | sort -k1,1 -k2,2 -k3,3n -k4,4g > "$R/samples.txt"

# The median of each (benchmark, variant, seed).
awk '
function flush() {
	if (n > 0)
		print key, (n % 2) ? a[(n + 1) / 2] : (a[n / 2] + a[n / 2 + 1]) / 2
	n = 0
}
{ k = $1 " " $2 " " $3; if (k != key) { flush(); key = k } a[++n] = $4 }
END { flush() }' "$R/samples.txt" > "$R/seed-medians.txt"

# Per (benchmark, variant): the default layout, and the minimum, median
# and maximum of the per-seed medians over all layouts.
sort -k1,1 -k2,2 -k4,4g "$R/seed-medians.txt" | awk '
function flush() {
	if (n > 0) {
		med = (n % 2) ? a[(n + 1) / 2] : (a[n / 2] + a[n / 2 + 1]) / 2
		printf "%-38s %-12s default %8.2f  min %8.2f  median %8.2f  max %8.2f  spread %5.1f%%\n",
			b, v, d, a[1], med, a[n], 100 * (a[n] - a[1]) / med
	}
	n = 0
	d = 0
}
{ if ($1 != b || $2 != v) { flush(); b = $1; v = $2 } a[++n] = $4; if ($3 == 0) d = $4 }
END { flush() }' > "$R/layout-summary.txt"

# All layouts of a variant pooled, compared with benchstat.
BENCHSTAT=${BENCHSTAT:-$(go env GOPATH)/bin/benchstat}
mkdir -p "$R/pooled"
for v in $VARIANTS; do
	cat "$R/$v"-[0-9]*.txt > "$R/pooled/$v.txt"
done
args=()
for v in $VARIANTS; do
	args+=("$v=$R/pooled/$v.txt")
done
"$BENCHSTAT" "${args[@]}" > "$R/pooled-benchstat.txt"
cat "$R/layout-summary.txt"
