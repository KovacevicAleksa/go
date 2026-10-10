# TEST / WIP -- benchmark only, not a change proposal

This branch exists only to run a benchmark on GitHub's 4-vCPU runners.
It is not meant for review, not sent to Gerrit, and will be deleted afterwards.

It compares experimental versions of the runtime on linux/amd64 and
linux/arm64, all against Go master at 3b98eddbcd:

- `base.diff`: g at 448 bytes (remove g.stackLock, valgrindStackID only in valgrind builds).
- `new.diff`: base plus packing the trace status flags and moving writebuf from g to m, g at 416 bytes.
- `new2.diff`: new with atomic.Or32 instead of a CAS loop when acquiring a trace status.
- `new3a.diff`: new2 with g.trace and g.gcAssistBytes moved off the end of g.
- `new3b.diff`: new3a with rarely used fields moved to the end of g.
- `ctl2.diff`: new2 with g padded back to 448 bytes, to separate the effect of the size class from the code.
- `basem.diff`: base with 32 bytes of padding in m after printlock, to see whether shifting m alone costs time.
- `new3bm.diff`: new3b with writebuf and writebufg at the end of m.
- `ctl.diff`: new padded back to 448 bytes; kept from the first run, no longer run.
- `new4.diff`: new3bm with 32-bit trace sequence counters next to atomicstatus and g.cgoCtxt allocated on first use, g at 384 bytes.
- `new4min.diff`: new4 with the field order of master except for the moves needed to reach 384 bytes, plus the trace parser comparing sequence counters modulo 2^32.
- `ctl4.diff`: new4min padded back to 416 bytes, to separate the effect of the size class from the code.
- `master`: Go at 3b98eddbcd with no change (no diff file).

`bench.sh` builds Go from source, builds the benchmark binaries for each
variant, runs them in turn, compares them with benchstat, and runs the tests
of one variant (see the workflow).
