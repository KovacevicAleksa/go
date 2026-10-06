#!/bin/bash
# Measures B (runtime: avoid an atomic store per goroutine creation) on the
# machine it runs on: builds Go at BASE from source, builds every benchmark
# binary once without and once with b.diff, runs old and new alternately,
# then compares with benchstat and runs go test -short runtime with the change.
#
# Usage: bench.sh <work dir> [rounds]
# Output: <work dir>/results/
set -euo pipefail

BASE=0b6b8381b6a8190c585cd454c747a501a5bfec7b
HERE=$(cd "$(dirname "$0")" && pwd)
W=$(mkdir -p "$1" && cd "$1" && pwd)
ROUNDS=${2:-20}
R=$W/results
mkdir -p "$R" "$W/old" "$W/new"
CPUS=${CPUS:-1,$(nproc)}

{
	date -u
	uname -a
	lscpu || true
	free -m || true
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

build() {
	local out=$1
	"$GO" test -c -o "$out/runtime.test" runtime
	"$GO" test -c -o "$out/http.test" net/http
	(cd "$HERE/spawn" && "$GO" build -o "$out/spawn" .)
	"$GO" tool objdump -s '^runtime.newproc1$' "$out/runtime.test" |
		grep -B4 -E 'XCHGB|STLRB' > "$out/newproc1-store.txt" || true
}
git -C "$W/go" checkout -q -- src/runtime/proc.go
build "$W/old"
git -C "$W/go" apply "$HERE/b.diff"
build "$W/new"
cp "$W/old/newproc1-store.txt" "$R/objdump-old.txt"
cp "$W/new/newproc1-store.txt" "$R/objdump-new.txt"

run() {
	local d=$1
	"$d/runtime.test" -test.run='^$' -test.bench='^BenchmarkCreateGoroutines' \
		-test.benchmem -test.cpu="$CPUS" >> "$R/runtime-$(basename "$d").txt"
	"$d/http.test" -test.run='^$' \
		-test.bench='^Benchmark(ServerFakeConnNoKeepAlive|ServerFakeConnWithKeepAliveLite|ClientServer)$' \
		-test.benchmem -test.cpu="$CPUS" >> "$R/http-$(basename "$d").txt"
	"$d/spawn" -cpu="$CPUS" >> "$R/spawn-$(basename "$d").txt"
}
for i in $(seq "$ROUNDS"); do
	echo "round $i/$ROUNDS"
	run "$W/old"
	run "$W/new"
done

BENCHSTAT=$(go env GOPATH)/bin/benchstat
for b in runtime http spawn; do
	"$BENCHSTAT" "$R/$b-old.txt" "$R/$b-new.txt" > "$R/$b-benchstat.txt"
done

# Correctness of the change on this platform; the tree still has b.diff applied.
if (cd "$W/go/src" && "$GO" test -short -count=1 runtime) > "$R/test-runtime.txt" 2>&1; then
	echo "go test -short runtime: ok" | tee -a "$R/test-runtime.txt"
else
	echo "go test -short runtime: FAIL" | tee -a "$R/test-runtime.txt"
	exit 1
fi
