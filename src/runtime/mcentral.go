// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
//
// The mcentral doesn't actually contain the list of free objects; the mspan does.
// Each mcentral is two lists of mspans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

package runtime

import "runtime/internal/atomic"

// todo 管理 span 的数据结构 (其 位于 spans 区？)
//
//		todo 全局的mspan缓存, 一共有67*2=134个
//
// todo central则是全局资源， 为多个线程服务
//		当某个线程内存不足时会向central申请， 当某个线程释放内存时又会回收进central
//
// Central list of free objects of a given size.  给定大小的空闲对象的 集中列表
//
// 有了管理内存的基本单位span， 还要有个数据结构来管理span， 这个数据结构叫mcentral， 各线程需要内存时从  `mcentral` 管理的 `span` 中申请内存
//
// todo 每个mcentral对象只管理特定的class规格的span
//		每种class都会对应一个 mcentral,这个mcentral的集合存放于mheap数据结构中
//
//go:notinheap
type mcentral struct {

	// 底层互斥锁
	lock      mutex

	// span class ID     (每个mcentral管理着一组有相同class的span列表)
	spanclass spanClass

	// non-empty 指还有空闲块 的span列表      (指还有内存可用的span列表)
	nonempty  mSpanList // list of spans with a free object, ie a nonempty free list

	// 指没有空闲块 的span列表		(指没有内存可用的span列表)
	empty     mSpanList // list of spans with no free objects (or cached in an mcache)

	// nmalloc is the cumulative count of objects allocated from
	// this mcentral, assuming all spans in mcaches are
	// fully-allocated. Written atomically, read under STW.
	//
	// 已累计分配的对象个数
	nmalloc uint64
}

// Initialize a single central free list.
func (c *mcentral) init(spc spanClass) {
	c.spanclass = spc
	c.nonempty.init()
	c.empty.init()
}

// todo 向mcentral申请一个新的span    (一般就是 交给 mcache 使用)
//
// Allocate a span to use in an mcache.   分配 span 以在mcache中使用
func (c *mcentral) cacheSpan() *mspan {

	// 让当前G协助一部分的sweep工作
	//
	// Deduct credit for this span allocation and sweep if necessary.    扣除 此 span 分配 的信用并在必要时进行扫描
	// 计算分配出来的 span 的大小
	spanBytes := uintptr(class_to_allocnpages[c.spanclass.sizeclass()]) * _PageSize
	deductSweepCredit(spanBytes, 0) // 扣除 此 span 分配 的信用并在必要时进行扫描

	// 对mcentral上锁, 因为可能会有多个M(P)同时访问
	lock(&c.lock)

	traceDone := false
	if trace.enabled {
		traceGCSweepStart()
	}
	sg := mheap_.sweepgen
retry:

	// mcentral里面有两个span的链表
	// 			- nonempty:  表示 确定 该span最少有一个未分配的元素
	// 			- empty:  表示 不确定 该span最少有一个未分配的元素
	//
	// 这里优先查找nonempty的链表
	// sweepgen每次GC都会增加2
	// 			- 当 (sweepgen == 全局sweepgen): 		表示span已经sweep过
	// 			- 当 (sweepgen == 全局sweepgen - 1): 	表示span正在sweep
	// 			- 当 (sweepgen == 全局sweepgen - 2): 	表示span等待sweep
	//
	var s *mspan

	// todo 先查找 noempty 链表
	for s = c.nonempty.first; s != nil; s = s.next {

		// 如果span等待sweep, 尝试原子修改sweepgen为全局sweepgen-1
		if s.sweepgen == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) {

			// 修改成功则把span移到empty链表, sweep它然后跳到havespan
			c.nonempty.remove(s)
			c.empty.insertBack(s)
			unlock(&c.lock)
			s.sweep(true)
			goto havespan
		}

		// 如果这个span正在被其他线程sweep, 就跳过
		if s.sweepgen == sg-1 {
			// the span is being swept by background sweeper, skip
			continue
		}

		// span已经sweep过
		// 因为nonempty链表中的span确定最少有一个未分配的元素, 这里可以直接使用它
		//
		// we have a nonempty span that does not require sweeping, allocate from it
		c.nonempty.remove(s)
		c.empty.insertBack(s)
		unlock(&c.lock)
		goto havespan
	}

	// todo 再查找 empty 链表
	for s = c.empty.first; s != nil; s = s.next {

		// 如果span等待sweep, 尝试原子修改sweepgen为全局sweepgen-1
		if s.sweepgen == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) {

			// 把span放到empty链表的最后
			//
			// we have an empty span that requires sweeping,
			// sweep it and see if we can free some space in it
			c.empty.remove(s)
			// swept spans are at the end of the list
			c.empty.insertBack(s)
			unlock(&c.lock)

			// 尝试sweep
			s.sweep(true)

			// sweep 后 还需要检测是否有未分配的对象, 如果有则可以使用它
			freeIndex := s.nextFreeIndex()
			if freeIndex != s.nelems {
				s.freeindex = freeIndex
				goto havespan
			}
			lock(&c.lock)
			// the span is still empty after sweep
			// it is already in the empty list, so just retry
			goto retry
		}

		// 如果这个span正在被其他线程sweep, 就跳过
		if s.sweepgen == sg-1 {
			// the span is being swept by background sweeper, skip
			continue
		}

		// 找不到有未分配对象的span
		//
		// already swept empty span,
		// all subsequent ones must also be either swept or in process of sweeping
		break
	}
	if trace.enabled {
		traceGCSweepDone()
		traceDone = true
	}
	unlock(&c.lock)

	// 找不到有未分配对象的span, 需要从mheap分配
	// 分配完成后加到 empty 链表中
	//
	// Replenish central list if empty.
	s = c.grow()
	if s == nil {
		return nil
	}
	lock(&c.lock)
	c.empty.insertBack(s)
	unlock(&c.lock)

	// At this point s is a non-empty span, queued at the end of the empty list,
	// c is unlocked.
havespan:
	if trace.enabled && !traceDone {
		traceGCSweepDone()
	}

	// 统计span中未分配的元素数量, 加到mcentral.nmalloc中
	// 统计span中未分配的元素总大小, 加到memstats.heap_live中
	n := int(s.nelems) - int(s.allocCount)
	if n == 0 || s.freeindex == s.nelems || uintptr(s.allocCount) == s.nelems {
		throw("span has no free objects")
	}
	// Assume all objects from this span will be allocated in the
	// mcache. If it gets uncached, we'll adjust this.
	atomic.Xadd64(&c.nmalloc, int64(n))
	usedBytes := uintptr(s.allocCount) * s.elemsize
	atomic.Xadd64(&memstats.heap_live, int64(spanBytes)-int64(usedBytes))
	if trace.enabled { // 跟踪处理
		// heap_live changed.
		traceHeapAlloc()
	}

	// 如果当前在GC中, 因为heap_live改变了, 重新调整G辅助标记工作的值
	// 详细请参考下面对revise函数的解析
	if gcBlackenEnabled != 0 {
		// heap_live changed.
		gcController.revise()  // 计算新的 辅助比率 用来算出 是否达到需要启动GC的内存分配量
	}

	// 根据freeindex更新allocCache
	freeByteBase := s.freeindex &^ (64 - 1)
	whichByte := freeByteBase / 8
	// Init alloc bits cache.
	s.refillAllocCache(whichByte)

	// Adjust the allocCache so that s.freeindex corresponds to the low bit in
	// s.allocCache.
	s.allocCache >>= s.freeindex % 64

	return s
}

// Return span from an mcache.
func (c *mcentral) uncacheSpan(s *mspan) {
	if s.allocCount == 0 {
		throw("uncaching span but s.allocCount == 0")
	}

	sg := mheap_.sweepgen
	stale := s.sweepgen == sg+1
	if stale {
		// Span was cached before sweep began. It's our
		// responsibility to sweep it.
		//
		// Set sweepgen to indicate it's not cached but needs
		// sweeping and can't be allocated from. sweep will
		// set s.sweepgen to indicate s is swept.
		atomic.Store(&s.sweepgen, sg-1)
	} else {
		// Indicate that s is no longer cached.
		atomic.Store(&s.sweepgen, sg)
	}

	n := int(s.nelems) - int(s.allocCount)
	if n > 0 {
		// cacheSpan updated alloc assuming all objects on s
		// were going to be allocated. Adjust for any that
		// weren't. We must do this before potentially
		// sweeping the span.
		atomic.Xadd64(&c.nmalloc, -int64(n))

		lock(&c.lock)
		c.empty.remove(s)
		c.nonempty.insert(s)
		if !stale {
			// mCentral_CacheSpan conservatively counted
			// unallocated slots in heap_live. Undo this.
			//
			// If this span was cached before sweep, then
			// heap_live was totally recomputed since
			// caching this span, so we don't do this for
			// stale spans.
			atomic.Xadd64(&memstats.heap_live, -int64(n)*int64(s.elemsize))
		}
		unlock(&c.lock)
	}

	if stale {
		// Now that s is in the right mcentral list, we can
		// sweep it.
		s.sweep(false)
	}
}

// freeSpan updates c and s after sweeping s.
// It sets s's sweepgen to the latest generation,
// and, based on the number of free objects in s,
// moves s to the appropriate list of c or returns it
// to the heap.
// freeSpan reports whether s was returned to the heap.
// If preserve=true, it does not move s (the caller
// must take care of it).
func (c *mcentral) freeSpan(s *mspan, preserve bool, wasempty bool) bool {
	if sg := mheap_.sweepgen; s.sweepgen == sg+1 || s.sweepgen == sg+3 {
		throw("freeSpan given cached span")
	}
	s.needzero = 1

	if preserve {
		// preserve is set only when called from (un)cacheSpan above,
		// the span must be in the empty list.
		if !s.inList() {
			throw("can't preserve unlinked span")
		}
		atomic.Store(&s.sweepgen, mheap_.sweepgen)
		return false
	}

	lock(&c.lock)

	// Move to nonempty if necessary.
	if wasempty {
		c.empty.remove(s)
		c.nonempty.insert(s)
	}

	// delay updating sweepgen until here. This is the signal that
	// the span may be used in an mcache, so it must come after the
	// linked list operations above (actually, just after the
	// lock of c above.)
	atomic.Store(&s.sweepgen, mheap_.sweepgen)

	if s.allocCount != 0 {
		unlock(&c.lock)
		return false
	}

	c.nonempty.remove(s)
	unlock(&c.lock)
	mheap_.freeSpan(s)
	return true
}

// todo mcentral向mheap申请一个新的span
//
// grow allocates a new empty span from the heap and initializes it for c's size class.
func (c *mcentral) grow() *mspan {

	// 根据mcentral的类型计算需要申请的span的大小(除以8K = 有多少页)和可以保存多少个元素
	npages := uintptr(class_to_allocnpages[c.spanclass.sizeclass()])
	size := uintptr(class_to_size[c.spanclass.sizeclass()])


	// 向mheap申请一个新的span, 以页(8K)为单位
	s := mheap_.alloc(npages, c.spanclass, true)
	if s == nil {
		return nil
	}

	// Use division by multiplication and shifts to quickly compute:
	// n := (npages << _PageShift) / size
	n := (npages << _PageShift) >> s.divShift * uintptr(s.divMul) >> s.divShift2
	s.limit = s.base() + size*n

	// 分配并 初始化span的 allocBits 和 gcmarkBits
	heapBitsForAddr(s.base()).initSpan(s)
	return s
}
