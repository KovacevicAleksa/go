# TEST / WIP -- benchmark only, not a change proposal

This branch exists only to run a benchmark on GitHub's 4-vCPU runners.
It is not meant for review, not sent to Gerrit, and will be deleted afterwards.

It measures an experimental runtime change (`b.diff`: check `g.runningCleanups`
before the atomic store in `newproc1`) on linux/amd64 and linux/arm64.
`bench.sh` builds Go at 0b6b8381b6 from source, builds the benchmark binaries
once without and once with `b.diff`, runs them alternately and compares them
with benchstat.
