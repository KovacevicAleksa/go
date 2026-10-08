#!/bin/bash
# Measures A3+A4 (g from 448 to 416 bytes) against A1+A2 (g at 448).
# Variants, all diffs against BASE:
#   base   A1+A2, g at 448
#   new    A3+A4 as first written (CAS loop in acquireStatus), g at 416
#   new2   new with atomic.Or32 in acquireStatus
#   new3a  new2 with g.trace and g.gcAssistBytes moved off the end of g
#   new3b  new3a with rarely used fields moved to the end of g
#   ctl2   new2 padded back to 448 bytes (same code, old size class)
# Builds Go at BASE from source, builds every benchmark binary once per
# variant, runs the variants in turn, compares them with benchstat, then
# runs the tests of TESTED (default new3b).
#
# Usage: bench.sh <work dir> [rounds]
# Output: <work dir>/results/
set -euo pipefail

BASE=3b98eddbcd66230a78c4893f32099b5d3045a334
VARIANTS=${VARIANTS:-base new new2 new3a new3b ctl2}
TESTED=${TESTED:-new3b}
HERE=$(cd "$(dirname "$0")" && pwd)
W=$(mkdir -p "$1" && cd "$1" && pwd)
ROUNDS=${2:-15}
R=$W/results
mkdir -p "$R"
CPUS=${CPUS:-1,2,$(nproc)}

{
	date -u
	uname -a
	lscpu || true
	free -m || true
	getconf LEVEL1_DCACHE_LINESIZE || true
	cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor 2>/dev/null || echo "no cpufreq governor"
} > "$R/machine.txt"

if [ ! -d "$W/go/.git" ]; then
	git init -q "$W/go"
	git -C "$W/go" fetch -q --depth 1 https://github.com/golang/go "$BASE"
	git -C "$W/go" checkout -q FETCH_HEAD
fi
(cd "$W/go/src" && ./make.bash)
GO=$W/go/bin/go
"$GO" version | tee "$R/go-version.txt"

reset_tree() {
	git -C "$W/go" checkout -q -- src
	git -C "$W/go" clean -fdq src
}

for v in $VARIANTS; do
	reset_tree
	git -C "$W/go" apply "$HERE/$v.diff"
	mkdir -p "$W/$v"
	"$GO" test -c -o "$W/$v/runtime.test" runtime
	"$GO" test -c -o "$W/$v/http.test" net/http
	(cd "$HERE/spawn" && "$GO" build -o "$W/$v/spawn" .)
	(cd "$HERE/memg" && "$GO" build -o "$W/$v/memg" .)
	# TestSizeof prints nothing on success; ctl2 is expected to fail it.
	"$W/$v/runtime.test" -test.run='^TestSizeof$' -test.v > "$R/sizeof-$v.txt" 2>&1 || true
done

run() {
	local v=$1 d=$W/$1
	"$d/runtime.test" -test.run='^$' \
		-test.bench='^Benchmark(CreateGoroutines|CreateGoroutinesParallel|CreateGoroutinesCapture|CreateGoroutinesSingle|Stack|StackParallel|StackAll|StackGrowth|StackGrowthDeep)$' \
		-test.benchmem -test.cpu="$CPUS" >> "$R/runtime-$v.txt"
	"$d/runtime.test" -test.run='^$' -test.bench='^BenchmarkGoroutineProfile$' \
		-test.benchmem -test.cpu="$(nproc)" >> "$R/runtime-$v.txt"
	"$d/http.test" -test.run='^$' \
		-test.bench='^Benchmark(ServerFakeConnNoKeepAlive|ServerFakeConnWithKeepAliveLite|ClientServer)$' \
		-test.benchmem -test.cpu="$CPUS" >> "$R/http-$v.txt"
	"$d/spawn" -cpu="$CPUS" >> "$R/spawn-$v.txt"
	"$d/spawn" -cpu="$CPUS" -trace >> "$R/spawn-$v.txt"
	"$d/memg" >> "$R/memg-$v.txt"
}
for i in $(seq "$ROUNDS"); do
	echo "round $i/$ROUNDS"
	for v in $VARIANTS; do
		run "$v"
	done
done

BENCHSTAT=$(go env GOPATH)/bin/benchstat
for b in runtime http spawn memg; do
	args=()
	for v in $VARIANTS; do
		args+=("$v=$R/$b-$v.txt")
	done
	"$BENCHSTAT" "${args[@]}" > "$R/$b-benchstat.txt"
	"$BENCHSTAT" ctl2="$R/$b-ctl2.txt" new2="$R/$b-new2.txt" new3a="$R/$b-new3a.txt" new3b="$R/$b-new3b.txt" > "$R/$b-benchstat-vs-ctl2.txt" || true
done

# Correctness of the tested variant on this platform.
reset_tree
git -C "$W/go" apply "$HERE/$TESTED.diff"
status=0
check() {
	local name=$1
	shift
	if (cd "$W/go/src" && "$@") > "$R/test-$name.txt" 2>&1; then
		echo "$name: ok" | tee -a "$R/tests.txt"
	else
		echo "$name: FAIL" | tee -a "$R/tests.txt"
		status=1
	fi
}
check vet "$GO" vet runtime
check sizeof "$GO" test -count=1 -run '^TestSizeof$' runtime
check runtime-short "$GO" test -short -count=1 runtime
check trace "$GO" test -count=1 internal/trace runtime/trace
check pprof-short "$GO" test -short -count=1 runtime/pprof
check asan-build "$GO" build -asan runtime
check race-short "$GO" test -race -short -count=1 -run 'Stack|Print|Hexdump|DebugLog|Trace' runtime
if [ "$(uname -m)" = x86_64 ]; then
	check sizeof-386 env GOARCH=386 "$GO" test -count=1 -run '^TestSizeof$' runtime
fi
exit $status
