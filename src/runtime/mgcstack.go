// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: stack objects and stack tracing
// See the design doc at https://docs.google.com/document/d/1un-Jn47yByHL7I0aVIP_uVCMxjdM5mpelJhiKlIqxkE/edit?usp=sharing
// Also see issue 22350.

// Stack tracing solves the problem of determining which parts of the
// stack are live and should be scanned. It runs as part of scanning
// a single goroutine stack.
//
// Normally determining which parts of the stack are live is easy to
// do statically, as user code has explicit references (reads and
// writes) to stack variables. The compiler can do a simple dataflow
// analysis to determine liveness of stack variables at every point in
// the code. See cmd/compile/internal/gc/plive.go for that analysis.
//
// However, when we take the address of a stack variable, determining
// whether that variable is still live is less clear. We can still
// look for static accesses, but accesses through a pointer to the
// variable are difficult in general to track statically. That pointer
// can be passed among functions on the stack, conditionally retained,
// etc.
//
// Instead, we will track pointers to stack variables dynamically.
// All pointers to stack-allocated variables will themselves be on the
// stack somewhere (or in associated locations, like defer records), so
// we can find them all efficiently.
//
// Stack tracing is organized as a mini garbage collection tracing
// pass. The objects in this garbage collection are all the variables
// on the stack whose address is taken, and which themselves contain a
// pointer. We call these variables "stack objects".
//
// We begin by determining all the stack objects on the stack and all
// the statically live pointers that may point into the stack. We then
// process each pointer to see if it points to a stack object. If it
// does, we scan that stack object. It may contain pointers into the
// heap, in which case those pointers are passed to the main garbage
// collection. It may also contain pointers into the stack, in which
// case we add them to our set of stack pointers.
//
// Once we're done processing all the pointers (including the ones we
// added during processing), we've found all the stack objects that
// are live. Any dead stack objects are not scanned and their contents
// will not keep heap objects live. Unlike the main garbage
// collection, we can't sweep the dead stack objects; they live on in
// a moribund state until the stack frame that contains them is
// popped.
//
// A stack can look like this:
//
// +----------+
// | foo()    |
// | +------+ |
// | |  A   | | <---\
// | +------+ |     |
// |          |     |
// | +------+ |     |
// | |  B   | |     |
// | +------+ |     |
// |          |     |
// +----------+     |
// | bar()    |     |
// | +------+ |     |
// | |  C   | | <-\ |
// | +----|-+ |   | |
// |      |   |   | |
// | +----v-+ |   | |
// | |  D  ---------/
// | +------+ |   |
// |          |   |
// +----------+   |
// | baz()    |   |
// | +------+ |   |
// | |  E  -------/
// | +------+ |
// |      ^   |
// | F: --/   |
// |          |
// +----------+
//
// foo() calls bar() calls baz(). Each has a frame on the stack.
// foo() has stack objects A and B.
// bar() has stack objects C and D, with C pointing to D and D pointing to A.
// baz() has a stack object E pointing to C, and a local variable F pointing to E.
//
// Starting from the pointer in local variable F, we will eventually
// scan all of E, C, D, and A (in that order). B is never scanned
// because there is no live pointer to it. If B is also statically
// dead (meaning that foo() never accesses B again after it calls
// bar()), then B's pointers into the heap are not considered live.

package runtime

import (
	"internal/goarch"
	"internal/runtime/sys"
	"unsafe"
)

const stackTraceDebug = false

// The buffers used while scanning a stack come from a pool owned by
// the P doing the scan (stackScanBufPool). Each buffer is
// stackScanBufPages pages, allocated as a manual span of its own.
const stackScanBufPages = 1

// Buffer for pointers found during stack tracing.
type stackWorkBuf struct {
	_ sys.NotInHeap
	stackWorkBufHdr
	obj [(stackScanBufPages*pageSize - unsafe.Sizeof(stackWorkBufHdr{})) / goarch.PtrSize]uintptr
}

// Header declaration must come after the buf declaration above, because of issue #14620.
type stackWorkBufHdr struct {
	_ sys.NotInHeap
	stackScanBufHdr
	next *stackWorkBuf // linked list of buffers
}

// Buffer for stack objects found on a goroutine stack.
type stackObjectBuf struct {
	_ sys.NotInHeap
	stackObjectBufHdr
	obj [(stackScanBufPages*pageSize - unsafe.Sizeof(stackObjectBufHdr{})) / unsafe.Sizeof(stackObject{})]stackObject
}

type stackObjectBufHdr struct {
	_ sys.NotInHeap
	stackScanBufHdr
	next *stackObjectBuf
}

// stackScanBufHdr is the header shared by stackWorkBuf and
// stackObjectBuf. Both kinds of buffer fit in a span of
// stackScanBufPages pages and come from the same pool.
type stackScanBufHdr struct {
	_        sys.NotInHeap
	nobj     int
	nextFree *stackScanBufHdr // next buffer in a stackScanBufPool
}

// A stackScanBufPool is a P's pool of free buffers for stack scanning:
// a LIFO list linked through stackScanBufHdr.nextFree. The P uses it
// without synchronization while scanning stacks in the mark phase.
// The pool grows to the largest number of buffers one scan on that P
// needed at once. After the mark phase the sweeper frees all of its
// buffers, see freeSomeStackScanBufs.
type stackScanBufPool struct {
	head *stackScanBufHdr
}

// get returns a buffer from the pool, allocating one from the heap if
// the pool is empty. The buffer must be returned with put before the
// scan ends.
//
//go:nowritebarrier
func (p *stackScanBufPool) get() *stackScanBufHdr {
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)

	if b := p.head; b != nil {
		p.head = b.nextFree
		b.nextFree = nil
		return b
	}
	var s *mspan
	systemstack(func() {
		s = mheap_.allocManual(stackScanBufPages, spanAllocStackScanBuf)
	})
	if s == nil {
		throw("out of memory")
	}
	b := (*stackScanBufHdr)(unsafe.Pointer(s.base()))
	b.nobj = 0
	b.nextFree = nil
	return b
}

// put returns an empty buffer taken with get to the pool.
//
//go:nowritebarrier
func (p *stackScanBufPool) put(b *stackScanBufHdr) {
	if b.nobj != 0 {
		throw("stackScanBufPool.put: buffer not empty")
	}
	b.nextFree = p.head
	p.head = b
}

// free frees up to n buffers from the pool to the heap and returns the
// number it freed, which is 0 once the pool is empty.
//
// This must only be called when gcphase == _GCoff, so that no stack
// is being scanned, and with preemption disabled or the world stopped,
// so that the phase cannot change during the call. See freeManual.
func (p *stackScanBufPool) free(n int) int {
	freed := 0
	systemstack(func() {
		for freed < n && p.head != nil {
			b := p.head
			p.head = b.nextFree
			mheap_.freeManual(spanOfUnchecked(uintptr(unsafe.Pointer(b))), spanAllocStackScanBuf)
			freed++
		}
	})
	return freed
}

// freeSomeStackScanBufs frees the buffers in every P's stack scan
// buffer pool, in batches. It returns true if it should be called
// again to free more.
//
// It is called by the background sweeper, concurrently with the
// mutator, and by gcSweep with the world stopped and preemptible
// false. The pools are only used by their Ps during the mark phase, so
// nothing else touches them while gcphase == _GCoff.
func freeSomeStackScanBufs(preemptible bool) bool {
	const batchSize = 64 // ~40 ns per buffer.
	// Disable preemption, so that once we have checked the phase it
	// cannot change under us: it only changes with the world stopped.
	// freeSomeWbufs and freeDeadSpanSPMCs get the same from their locks.
	mp := acquirem()
	if gcphase != _GCoff {
		releasem(mp)
		return false
	}
	gp := mp.curg
	n := 0
	for _, pp := range allp {
		for {
			if n == batchSize || (preemptible && gp.preempt) {
				releasem(mp)
				return true
			}
			freed := pp.gcw.stackBufs.free(batchSize - n)
			if freed == 0 {
				break
			}
			n += freed
		}
	}
	releasem(mp)
	return false
}

// A stackObject represents a variable on the stack that has had
// its address taken.
type stackObject struct {
	_     sys.NotInHeap
	off   uint32             // offset above stack.lo
	size  uint32             // size of object
	r     *stackObjectRecord // info of the object (for ptr/nonptr bits). nil if object has been scanned.
	left  *stackObject       // objects with lower addresses
	right *stackObject       // objects with higher addresses
}

// obj.r = r, but with no write barrier.
//
//go:nowritebarrier
func (obj *stackObject) setRecord(r *stackObjectRecord) {
	// Types of stack objects are always in read-only memory, not the heap.
	// So not using a write barrier is ok.
	*(*uintptr)(unsafe.Pointer(&obj.r)) = uintptr(unsafe.Pointer(r))
}

// A stackScanState keeps track of the state used during the GC walk
// of a goroutine.
type stackScanState struct {
	// stack limits
	stack stack

	// pool is the stack scan buffer pool of the P running this scan.
	// The pointer and object buffers below are taken from and
	// returned to it.
	pool *stackScanBufPool

	// conservative indicates that the next frame must be scanned conservatively.
	// This applies only to the innermost frame at an async safe-point.
	conservative bool

	// buf contains the set of possible pointers to stack objects.
	// Organized as a LIFO linked list of buffers.
	// All buffers except possibly the head buffer are full.
	buf     *stackWorkBuf
	freeBuf *stackWorkBuf // keep around one free buffer for allocation hysteresis

	// cbuf contains conservative pointers to stack objects. If
	// all pointers to a stack object are obtained via
	// conservative scanning, then the stack object may be dead
	// and may contain dead pointers, so it must be scanned
	// defensively.
	cbuf *stackWorkBuf

	// list of stack objects
	// Objects are in increasing address order.
	head  *stackObjectBuf
	tail  *stackObjectBuf
	nobjs int

	// root of binary tree for fast object lookup by address
	// Initialized by buildIndex.
	root *stackObject
}

// Add p as a potential pointer to a stack object.
// p must be a stack address.
func (s *stackScanState) putPtr(p uintptr, conservative bool) {
	if p < s.stack.lo || p >= s.stack.hi {
		throw("address not a stack address")
	}
	head := &s.buf
	if conservative {
		head = &s.cbuf
	}
	buf := *head
	if buf == nil {
		// Initial setup.
		buf = (*stackWorkBuf)(unsafe.Pointer(s.pool.get()))
		buf.nobj = 0
		buf.next = nil
		*head = buf
	} else if buf.nobj == len(buf.obj) {
		if s.freeBuf != nil {
			buf = s.freeBuf
			s.freeBuf = nil
		} else {
			buf = (*stackWorkBuf)(unsafe.Pointer(s.pool.get()))
		}
		buf.nobj = 0
		buf.next = *head
		*head = buf
	}
	buf.obj[buf.nobj] = p
	buf.nobj++
}

// Remove and return a potential pointer to a stack object.
// Returns 0 if there are no more pointers available.
//
// This prefers non-conservative pointers so we scan stack objects
// precisely if there are any non-conservative pointers to them.
func (s *stackScanState) getPtr() (p uintptr, conservative bool) {
	for _, head := range []**stackWorkBuf{&s.buf, &s.cbuf} {
		buf := *head
		if buf == nil {
			// Never had any data.
			continue
		}
		if buf.nobj == 0 {
			if s.freeBuf != nil {
				// Free old freeBuf.
				s.pool.put((*stackScanBufHdr)(unsafe.Pointer(s.freeBuf)))
			}
			// Move buf to the freeBuf.
			s.freeBuf = buf
			buf = buf.next
			*head = buf
			if buf == nil {
				// No more data in this list.
				continue
			}
		}
		buf.nobj--
		return buf.obj[buf.nobj], head == &s.cbuf
	}
	// No more data in either list.
	if s.freeBuf != nil {
		s.pool.put((*stackScanBufHdr)(unsafe.Pointer(s.freeBuf)))
		s.freeBuf = nil
	}
	return 0, false
}

// addObject adds a stack object at addr of type typ to the set of stack objects.
func (s *stackScanState) addObject(addr uintptr, r *stackObjectRecord) {
	x := s.tail
	if x == nil {
		// initial setup
		x = (*stackObjectBuf)(unsafe.Pointer(s.pool.get()))
		x.next = nil
		s.head = x
		s.tail = x
	}
	if x.nobj > 0 && uint32(addr-s.stack.lo) < x.obj[x.nobj-1].off+x.obj[x.nobj-1].size {
		throw("objects added out of order or overlapping")
	}
	if x.nobj == len(x.obj) {
		// full buffer - allocate a new buffer, add to end of linked list
		y := (*stackObjectBuf)(unsafe.Pointer(s.pool.get()))
		y.next = nil
		x.next = y
		s.tail = y
		x = y
	}
	obj := &x.obj[x.nobj]
	x.nobj++
	obj.off = uint32(addr - s.stack.lo)
	obj.size = uint32(r.size)
	obj.setRecord(r)
	// obj.left and obj.right will be initialized by buildIndex before use.
	s.nobjs++
}

// buildIndex initializes s.root to a binary search tree.
// It should be called after all addObject calls but before
// any call of findObject.
func (s *stackScanState) buildIndex() {
	s.root, _, _ = binarySearchTree(s.head, 0, s.nobjs)
}

// Build a binary search tree with the n objects in the list
// x.obj[idx], x.obj[idx+1], ..., x.next.obj[0], ...
// Returns the root of that tree, and the buf+idx of the nth object after x.obj[idx].
// (The first object that was not included in the binary search tree.)
// If n == 0, returns nil, x.
func binarySearchTree(x *stackObjectBuf, idx int, n int) (root *stackObject, restBuf *stackObjectBuf, restIdx int) {
	if n == 0 {
		return nil, x, idx
	}
	var left, right *stackObject
	left, x, idx = binarySearchTree(x, idx, n/2)
	root = &x.obj[idx]
	idx++
	if idx == len(x.obj) {
		x = x.next
		idx = 0
	}
	right, x, idx = binarySearchTree(x, idx, n-n/2-1)
	root.left = left
	root.right = right
	return root, x, idx
}

// findObject returns the stack object containing address a, if any.
// Must have called buildIndex previously.
func (s *stackScanState) findObject(a uintptr) *stackObject {
	off := uint32(a - s.stack.lo)
	obj := s.root
	for {
		if obj == nil {
			return nil
		}
		if off < obj.off {
			obj = obj.left
			continue
		}
		if off >= obj.off+obj.size {
			obj = obj.right
			continue
		}
		return obj
	}
}
