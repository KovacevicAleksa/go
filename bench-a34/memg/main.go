// Command memg measures the memory cost of parked goroutines, the case
// where the size of g matters most. Output is in Go benchmark format.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"sync"
)

var n = flag.Int("n", 100000, "parked goroutines")

func main() {
	flag.Parse()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	var started sync.WaitGroup
	block := make(chan struct{})
	started.Add(*n)
	for range *n {
		go func() {
			started.Done()
			<-block
		}()
	}
	started.Wait()
	runtime.GC()
	runtime.ReadMemStats(&after)

	per := func(a, b uint64) float64 { return float64(int64(a)-int64(b)) / float64(*n) }
	fmt.Printf("BenchmarkParkedGoroutines\t%d\t%.1f heap-B/goroutine\t%.1f stack-B/goroutine\t%.1f sys-B/goroutine\t%.3f mallocs/goroutine\n",
		*n, per(after.HeapAlloc, before.HeapAlloc), per(after.StackInuse, before.StackInuse),
		per(after.Sys, before.Sys), per(after.Mallocs, before.Mallocs))
	close(block)
}
