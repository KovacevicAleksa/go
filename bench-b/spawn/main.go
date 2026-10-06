// Command spawn measures goroutine creation the way B changes it: the time
// of every single go statement (percentiles, not only the mean) and memory.
// Output is in Go benchmark format, so benchstat can compare two runs.
//
// Patterns:
//
//	Accept:   one goroutine starts every goroutine, like an accept loop.
//	Parallel: GOMAXPROCS goroutines start goroutines at the same time.
//
// Each pattern runs once to warm the free g list, then once measured.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	n    = flag.Int("n", 200000, "goroutines per measured run")
	cpus = flag.String("cpu", "1", "comma-separated GOMAXPROCS values")
)

func main() {
	flag.Parse()
	for _, s := range strings.Split(*cpus, ",") {
		p, err := strconv.Atoi(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		runtime.GOMAXPROCS(p)
		accept(*n) // warm-up
		report("SpawnAccept", p, accept(*n))
		parallel(*n, p) // warm-up
		report("SpawnParallel", p, parallel(*n, p))
	}
	runtime.GOMAXPROCS(1)
	memory()
}

type result struct {
	elapsed time.Duration
	lat     []int64 // duration of each go statement, ns
}

func accept(n int) result {
	lat := make([]int64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	start := time.Now()
	for i := range n {
		t0 := time.Now()
		go wg.Done()
		lat[i] = int64(time.Since(t0))
	}
	wg.Wait()
	return result{time.Since(start), lat}
}

func parallel(n, p int) result {
	lat := make([]int64, n)
	per := n / p
	var wg sync.WaitGroup
	wg.Add(per * p)
	var ready sync.WaitGroup
	ready.Add(p)
	gate := make(chan struct{})
	for w := range p {
		go func() {
			ready.Done()
			<-gate
			for i := w * per; i < (w+1)*per; i++ {
				t0 := time.Now()
				go wg.Done()
				lat[i] = int64(time.Since(t0))
			}
		}()
	}
	ready.Wait()
	start := time.Now()
	close(gate)
	wg.Wait()
	return result{time.Since(start), lat[:per*p]}
}

func report(name string, p int, r result) {
	slices.Sort(r.lat)
	q := func(f float64) int64 { return r.lat[int(f*float64(len(r.lat)-1))] }
	fmt.Printf("Benchmark%s-%d\t%d\t%.2f ns/op\t%d p50-go-ns\t%d p90-go-ns\t%d p99-go-ns\t%d p99.9-go-ns\t%d max-go-ns\n",
		name, p, len(r.lat), float64(r.elapsed.Nanoseconds())/float64(len(r.lat)),
		q(0.50), q(0.90), q(0.99), q(0.999), r.lat[len(r.lat)-1])
}

func memory() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	// On Linux Maxrss is in KiB.
	fmt.Printf("BenchmarkSpawnMemory\t1\t%d maxrss-KiB\t%d sys-B\t%d stack-sys-B\t%d heap-sys-B\t%d gcs\t%d gc-pause-total-ns\n",
		ru.Maxrss, ms.Sys, ms.StackSys, ms.HeapSys, ms.NumGC, ms.PauseTotalNs)
}
