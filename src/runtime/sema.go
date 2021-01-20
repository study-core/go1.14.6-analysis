// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
// Thus it targets the same goal as Linux's futex,
// but it has much simpler semantics.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
//
// See Mullender and Cox, ``Semaphores in Plan 9,''
// https://swtch.com/semaphore.pdf

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

// Asynchronous semaphore for sync.Mutex.
// 用于sync.Mutex的异步信号量

// A semaRoot holds a balanced tree of sudog with distinct addresses (s.elem).
// Each of those sudog may in turn point (through s.waitlink) to a list
// of other sudogs waiting on the same address.
// The operations on the inner lists of sudogs with the same address
// are all O(1). The scanning of the top-level semaRoot list is O(log n),
// where n is the number of distinct addresses with goroutines blocked
// on them that hash to the given semaRoot.
// See golang.org/issue/17953 for a program that worked badly
// before we introduced the second level of list, and test/locklinear.go
// for a test that exercises this.

// todo semaRoot 拥有一个具有不同地址（s.elem）的 sudog 平衡树
//	每个sudog都可以依次（通过s.waitlink）指向在同一地址上等待的其他sudog列表。
//	对具有相同地址的sudog内部列表进行的操作均为O（1）。 顶层semaRoot列表的扫描为O（log n），其中n是在其上阻止了goroutine并散列到给定semaRoot的不同地址的数量
//	请参阅golang.org/issue/17953，了解在引入第二级列表之前运行不佳的程序，以及test / locklinear.go，了解用于执行此功能的测试的信息
//
// 一个 semaRoot 持有不同地址的 sudog 的平衡树
// 每一个 sudog 可能反过来指向等待在同一个地址上的 sudog 的列表
// 对同一个地址上的 sudog 的内联列表的操作的时间复杂度都是O(1)，扫描 semaRoot 的顶部列表是 O(log n)
// n 是 hash 到并且阻塞在同一个 semaRoot 上的不同地址的 goroutines 的总数
type semaRoot struct {
	lock  mutex		// 这个才是真的 互斥锁
	treap *sudog // root of balanced tree of unique waiters.   唯一的 等待队列 g 的平衡树 root   todo 一个 放了 sodug 的 树堆
	nwait uint32 // Number of waiters. Read w/o the lock.   等待队列的 g 数目
}

// Prime to not correlate with any user patterns.
const semTabSize = 251

// 一个全局的  sema 表 ( 251 元素的数组)    可以有 251 个信号量 Treap同时存在
var semtable [semTabSize]struct {
	root semaRoot
	pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte   // 做对齐 填充用
}

//go:linkname sync_runtime_Semacquire sync.runtime_Semacquire
func sync_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0)
}

//go:linkname poll_runtime_Semacquire internal/poll.runtime_Semacquire
func poll_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0)
}

// todo 信号量 去释放 当前 g
//go:linkname sync_runtime_Semrelease sync.runtime_Semrelease
func sync_runtime_Semrelease(addr *uint32, handoff bool, skipframes int) {
	semrelease1(addr, handoff, skipframes)
}

// todo 信号量 去阻塞 当前 g
//go:linkname sync_runtime_SemacquireMutex sync.runtime_SemacquireMutex
func sync_runtime_SemacquireMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes)
}

//go:linkname poll_runtime_Semrelease internal/poll.runtime_Semrelease
func poll_runtime_Semrelease(addr *uint32) {
	semrelease(addr)
}


//	readyWithTime 把 sudog 对应的 g 唤醒，并且放到 p 本地队列的下一个执行位置
//	readWithTime 会调用 systemstack , systemstack 会切换到当前 os 线程的堆栈执行 read
func readyWithTime(s *sudog, traceskip int) {
	if s.releasetime != 0 {
		s.releasetime = cputicks()
	}
	goready(s.g, traceskip)
}

type semaProfileFlags int

const (
	semaBlockProfile semaProfileFlags = 1 << iota
	semaMutexProfile
)

// Called from runtime.
func semacquire(addr *uint32) {
	semacquire1(addr, false, 0, 0)
}

// todo 阻塞 信号量
//
// 大致流程：
//
// 获取 sudog 和 semaRoot ，
//
// 其中 sudog 是 g 放在等待队列里的包装对象，sudog 里会有 g 的信息和一些其他的参数，
//
// semaRoot 则是 队列结构体，内部是 treap ，把和当前 g 关联的 sudog 放到 semaRoot 里，
// 		然后把 g 的状态改为【等待】，调用调度器执行别的 g，此时当前 g 就停止执行了。
// 		一直到被调度器重新调度执行，会首先释放 sudog 然后再去执行别的代码逻辑。
//
//
// 入参说明:
//
// 	addr:  用来定位 信号灯 的 Treap 变量 (变量占用内存的哦, 故取名 addr)
// 	lifo:  表示  是否将当前入队的G放到 等待队列头部,  LIFO 后进先出
//	profile:
//	skipframes:
//
func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int) {

	// 获取 当前G  (当前 G 需要被 阻塞 挂起)
	gp := getg()
	if gp != gp.m.curg {
		throw("semacquire not on the G stack")
	}

	// Easy case.
	if cansemacquire(addr) {
		return
	}

	// Harder case:
	//	increment waiter count
	//	try cansemacquire one more time, return if succeeded
	//	enqueue itself as a waiter
	//	sleep
	//	(waiter descriptor is dequeued by signaler)
	//
	//
	// 更难的情况：
	//    增加 waiter 人数
	//    再尝试一次 cansemacquire，如果成功则返回
	//    使自己成为 waiter
	//    sleep
	//    ( waiter 描述符由 signaler 出队)
	//
	//
	s := acquireSudog()    	// todo 获取一个 sudog
	root := semroot(addr)	// todo 获取一个 semaRoot
	t0 := int64(0)
	s.releasetime = 0
	s.acquiretime = 0  	// 开始阻塞的时间 <也是 treap 上的 key>
	s.ticket = 0  		// g 的 票号  <也是 treap 上的 优先级>

	// 一些性能采集的参数 应该是
	if profile&semaBlockProfile != 0 && blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}
	if profile&semaMutexProfile != 0 && mutexprofilerate > 0 {
		if t0 == 0 {
			t0 = cputicks()
		}
		s.acquiretime = t0
	}


	for {

		// 锁定在 semaRoot 上
		lock(&root.lock)
		// Add ourselves to nwait to disable "easy case" in semrelease.
		// nwait 加一
		atomic.Xadd(&root.nwait, 1)
		// Check cansemacquire to avoid missed wakeup.   检查 cansemacquire() 避免错过唤醒
		if cansemacquire(addr) {
			atomic.Xadd(&root.nwait, -1)
			unlock(&root.lock)
			break
		}
		// Any semrelease after the cansemacquire knows we're waiting
		// (we set nwait above), so go to sleep.
		//
		root.queue(addr, s, lifo) // todo 加到 semaRoot treap 上

		// 解锁 semaRoot ，todo 并且把当前 g 的状态改为等待，然后让当前的 m 调用其他的 g 执行，当前 g 相当于等待
		goparkunlock(&root.lock, waitReasonSemacquire, traceEvGoBlockSync, 4+skipframes)
		if s.ticket != 0 || cansemacquire(addr) {
			break
		}
	}
	if s.releasetime > 0 {
		blockevent(s.releasetime-t0, 3+skipframes)
	}

	// 释放 sudog
	releaseSudog(s)
}

func semrelease(addr *uint32) {
	semrelease1(addr, false, 0)
}


// todo 释放 信号量

// 大致流程：
//
// 设置 addr 信号，从队列里取 sudog s，
// 把 s 对应的 g 变为【可执行状态】，
// 并且放在 p 的本地队列   下一个  执行的位置。
//
//  如果参数 handoff 为 true，并且当前 m.locks == 0，就把当前的 g 放到 p 本地队列的队尾，
// 	调用调度器，因为s.g 被放到 p 本地队列的下一个执行位置，所以调度器此刻执行的就是 s.g
//
func semrelease1(addr *uint32, handoff bool, skipframes int) {

	// 取出 addr变量对应的 Treap的 root
	root := semroot(addr)

	// 当在 semacquire1() 中 调用了 cansemacquire() 对 addr -1 操作后, 这里需要将它 +1   (好比 信号一来一去)
	atomic.Xadd(addr, 1)

	// Easy case: no waiters?
	// This check must happen after the xadd, to avoid a missed wakeup
	// (see loop in semacquire).
	//
	//
	// 简单的例子：没有 waiters ?
	// 此检查必须在 xadd 之后进行，以避免错过唤醒（请参阅semacquire中的循环）
	//
	//
	// 没有等待的 sudog
	if atomic.Load(&root.nwait) == 0 {
		return
	}

	// Harder case: search for a waiter and wake it.   更难的情况：搜索 waiter 并唤醒它
	lock(&root.lock)
	if atomic.Load(&root.nwait) == 0 {
		// The count is already consumed by another goroutine,
		// so no need to wake up another goroutine.
		//
		// 该 计数 已被另一个goroutine占用，因此无需唤醒另一个goroutine
		unlock(&root.lock)
		return
	}

	// 从队列里取出来 sudog ，此时 ticket == 0
	s, t0 := root.dequeue(addr)
	if s != nil {
		atomic.Xadd(&root.nwait, -1)   // 递减 wait 计数
	}
	unlock(&root.lock)
	if s != nil { // May be slow or even yield, so unlock first
		acquiretime := s.acquiretime
		if acquiretime != 0 {
			mutexevent(t0-acquiretime, 3+skipframes)
		}
		if s.ticket != 0 {
			throw("corrupted semaphore ticket")
		}
		if handoff && cansemacquire(addr) {
			s.ticket = 1
		}

		// todo 唤醒 sodug对应的 g
		//
		// 把 sudog 对应的 g 改为待执行状态，并且放到 p 本地队列的下一个执行
		//
		//	readyWithTime 把 sudog 对应的 g 唤醒，并且放到 p 本地队列的下一个执行位置
		//	readWithTime 会调用 systemstack , systemstack 会切换到当前 os 线程的堆栈执行 read
		readyWithTime(s, 5+skipframes)
		if s.ticket == 1 && getg().m.locks == 0 {
			// Direct G handoff
			// readyWithTime has added the waiter G as runnext in the
			// current P; we now call the scheduler so that we start running
			// the waiter G immediately.
			// Note that waiter inherits our time slice: this is desirable
			// to avoid having a highly contended semaphore hog the P
			// indefinitely. goyield is like Gosched, but it emits a
			// "preempted" trace event instead and, more importantly, puts
			// the current G on the local runq instead of the global one.
			// We only do this in the starving regime (handoff=true), as in
			// the non-starving case it is possible for a different waiter
			// to acquire the semaphore while we are yielding/scheduling,
			// and this would be wasteful. We wait instead to enter starving
			// regime, and then we start to do direct handoffs of ticket and
			// P.
			// See issue 33747 for discussion.
			//
			//
			// todo 直接 G 切换
			//
			// `readyWithTime()` 已在当前P中将 waiter G添加为runnext; 我们现在调用调度程序，以便我们立即开始运行 waiter G
			// 注意，waiter 继承了我们的时间片：
			// 			这是避免避免竞争激烈的信号量无限期地占用P的理想选择. `goyield()` 类似于Gosched，但是它发出“被抢占”的跟踪事件，
			// 			更重要的是，将当前G放在 P的本地runq 而不是 schedt的全局runq上
			//
			// 我们仅在【饥饿状态】下执行此操作 (handoff = true)，
			// 因为在【非饥饿状态】下，当我们进行 屈服/调度 (yielding/scheduling) 时，可能有其他 waiter 获取信号量，这很浪费.
			// 							我们等待进入【饥饿状态】，然后开始进行 票号 和 P 的直接交接
			//
			// 参见问题33747进行讨论
			//
			//
			// 调用调度器立即执行 G
			// 等待的 g 继承时间片，避免无限制的争夺信号量
			// 把当前 g 放到 p 本地队列的队尾，启动调度器，因为 s.g 在本地队列的下一个，所以调度器立马执行 s.g
			//
			// goyield 调用 mcall 执行 goyield_m， todo goyield_m 会把当前的 g 放到 p 本地对象的队尾， 然后执行调度器 发起新的【一轮调度】
			goyield()
		}
	}
}

// 获取 信号灯 上的 Treap的 root
func semroot(addr *uint32) *semaRoot {

	// 去 addr 变量对应的指针的值,  算出他在哪个 Treap
	return &semtable[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root    // Treap 的 key 是 sudog 的 acquiretime， 而 优先级 是 ticket
}


// 判断是否可以获取信号灯
//
// 之所以要 变动 addr 的值, 是因为 addr 将作为 Treap 中 key
func cansemacquire(addr *uint32) bool {
	for {
		v := atomic.Load(addr)
		if v == 0 {
			return false
		}

		// 由于 addr 是 uint, 所以 -1 操作只会让他变成一个   超级大的数
		//
		if atomic.Cas(addr, v, v-1) {  // 配合 for 进行多次尝试 修改
			return true
		}
	}
}

// todo sudog 入队信号量sema的Treap
//
// 将当前 G 封装成 sudog，并加入 sema的 树堆 Treap 中
//
// queue adds s to the blocked goroutines in semaRoot.
//
// 队列将s添加到semaRoot中被阻止的goroutine中
//
func (root *semaRoot) queue(addr *uint32, s *sudog, lifo bool) {

	// 将当前g 封入 sudog
	s.g = getg()
	s.elem = unsafe.Pointer(addr)  // 将当前 信号量 赋予 elem 内存指针
	s.next = nil
	s.prev = nil

	var last *sudog
	pt := &root.treap   // 取出 root对应的 Treap的 头元素
	for t := *pt; t != nil; t = *pt {

		// 如果是 重置  sudog 的 内容时
		if t.elem == unsafe.Pointer(addr) {
			// Already have addr in list.  队列中已经有 地址

			// 如果需要 后进先出
			if lifo {
				// Substitute s in t's place in treap. 用treap 代替 s

				// 将当前入队的 Treap 放到 Treap 的头部
				*pt = s
				s.ticket = t.ticket   // todo 后进先出的话, 将之前 Treap 顶部的 t 的票号 赋值给 当前 sudog
				s.acquiretime = t.acquiretime
				s.parent = t.parent
				s.prev = t.prev
				s.next = t.next
				if s.prev != nil {
					s.prev.parent = s
				}
				if s.next != nil {
					s.next.parent = s
				}
				// Add t first in s's wait list.
				s.waitlink = t  // 将之前的 Treap 头元素 接到当前 sudog 后面
				s.waittail = t.waittail
				if s.waittail == nil {
					s.waittail = t
				}
				t.parent = nil
				t.prev = nil
				t.next = nil
				t.waittail = nil

			// 不需要  后进先出时
			} else {
				// Add s to end of t's wait list.
				if t.waittail == nil {
					t.waitlink = s
				} else {
					t.waittail.waitlink = s
				}
				t.waittail = s
				s.waitlink = nil
			}

			// 重置完, 直接返回
			return
		}
		last = t

		// 使用 elem 的
		if uintptr(unsafe.Pointer(addr)) < uintptr(t.elem) {
			pt = &t.prev
		} else {
			pt = &t.next
		}
	}

	// Add s as new leaf in tree of unique addrs.
	// The balanced tree is a treap using ticket as the random heap priority.
	// That is, it is a binary tree ordered according to the elem addresses,
	// but then among the space of possible binary trees respecting those
	// addresses, it is kept balanced on average by maintaining a heap ordering
	// on the ticket: s.ticket <= both s.prev.ticket and s.next.ticket.
	// https://en.wikipedia.org/wiki/Treap
	// https://faculty.washington.edu/aragon/pubs/rst89.pdf
	//
	// s.ticket compared with zero in couple of places, therefore set lowest bit.
	// It will not affect treap's quality noticeably.
	//
	//
	// 将 s 作为 新叶子 添加到唯一地址树中
	// 平衡树是使用 票证作为随机堆优先级的 Treap
	// 这是一个根据 elem 地址 排序的二叉树，但是在 尊重这些地址的可能二叉树的空间中，通过在 票证上保持堆排序来平均地保持平衡：s.ticket <= s.prev.ticket和s.next.ticket
	//  https://en.wikipedia.org/wiki/Treap
	//  https://faculty.washington.edu/aragon/pubs/rst89.pdf
	//
	//  s.ticket在几个地方与零相比，因此设置了最低位
	// 不会明显影响 Treap 的质量
	//
	//

	// todo 随机生成 票号  作为 sudog 在 Treap 中的优先级
	s.ticket = fastrand() | 1
	s.parent = last
	*pt = s

	// todo 调整 Treap
	//
	// Rotate up into tree according to ticket (priority).   是用了 最小堆 ??  (根据 ticket 票号作为优先级)
	for s.parent != nil && s.parent.ticket > s.ticket {
		if s.parent.prev == s {
			root.rotateRight(s.parent)
		} else {
			if s.parent.next != s {
				panic("semaRoot queue")
			}
			root.rotateLeft(s.parent)
		}
	}
}

// todo 从信号量sema的Treap中出队一个 sodug
//
// dequeue searches for and finds the first goroutine
// in semaRoot blocked on addr.
// If the sudog was being profiled, dequeue returns the time
// at which it was woken up as now. Otherwise now is 0.
//
//
// 出队搜索并在 addr上阻止的 semaRoot 中找到第一个goroutine
// 如果对 sudog进行了概要分析，则出队返回到现在为止唤醒它的时间.  否则现在为 0
//
func (root *semaRoot) dequeue(addr *uint32) (found *sudog, now int64) {

	// 取出
	ps := &root.treap
	s := *ps

	// 普通二叉树查找 Treap
	for ; s != nil; s = *ps {
		if s.elem == unsafe.Pointer(addr) {
			goto Found
		}
		if uintptr(unsafe.Pointer(addr)) < uintptr(s.elem) {
			ps = &s.prev
		} else {
			ps = &s.next
		}
	}
	return nil, 0

Found:   // 找到 目标 sudog 后
	now = int64(0)
	if s.acquiretime != 0 {
		now = cputicks()
	}

	// todo  调整 Treap

	// 取出 sudog 的下一个 sudog (就是t)
	if t := s.waitlink; t != nil {
		// Substitute t, also waiting on addr, for s in root tree of unique addrs.
		//
		// 用 t 代替唯一的 addrs 的 root树中的s，也等待addr
		*ps = t
		t.ticket = s.ticket
		t.parent = s.parent
		t.prev = s.prev
		if t.prev != nil {
			t.prev.parent = t
		}
		t.next = s.next
		if t.next != nil {
			t.next.parent = t
		}
		if t.waitlink != nil {
			t.waittail = s.waittail
		} else {
			t.waittail = nil
		}
		t.acquiretime = now
		s.waitlink = nil
		s.waittail = nil

	// 如果当前 sudog 在Treap中没有 下一个 sudog
	} else {
		// Rotate s down to be leaf of tree for removal, respecting priorities.
		for s.next != nil || s.prev != nil {
			if s.next == nil || s.prev != nil && s.prev.ticket < s.next.ticket {
				root.rotateRight(s)
			} else {
				root.rotateLeft(s)
			}
		}
		// Remove s, now a leaf.
		if s.parent != nil {
			if s.parent.prev == s {
				s.parent.prev = nil
			} else {
				s.parent.next = nil
			}
		} else {
			root.treap = nil
		}
	}
	s.parent = nil
	s.elem = nil
	s.next = nil
	s.prev = nil
	s.ticket = 0   // 清除 sodug 的票号
	return s, now
}

// rotateLeft rotates the tree rooted at node x.
// turning (x a (y b c)) into (y (x a b) c).
func (root *semaRoot) rotateLeft(x *sudog) {
	// p -> (x a (y b c))
	p := x.parent
	y := x.next
	b := y.prev

	y.prev = x
	x.parent = y
	x.next = b
	if b != nil {
		b.parent = x
	}

	y.parent = p
	if p == nil {
		root.treap = y
	} else if p.prev == x {
		p.prev = y
	} else {
		if p.next != x {
			throw("semaRoot rotateLeft")
		}
		p.next = y
	}
}

// rotateRight rotates the tree rooted at node y.
// turning (y (x a b) c) into (x a (y b c)).
func (root *semaRoot) rotateRight(y *sudog) {
	// p -> (y (x a b) c)
	p := y.parent
	x := y.prev
	b := x.next

	x.next = y
	y.parent = x
	y.prev = b
	if b != nil {
		b.parent = y
	}

	x.parent = p
	if p == nil {
		root.treap = x
	} else if p.prev == y {
		p.prev = x
	} else {
		if p.next != y {
			throw("semaRoot rotateRight")
		}
		p.next = x
	}
}

// notifyList is a ticket-based notification list used to implement sync.Cond.
//
// It must be kept in sync with the sync package.
//
//
// notifyList 是基于 `票证` 的通知列表，用于实现 sync.Cond.go 中的 notifyList 字段
//
// todo 结构 必须要和 runtime.go 中的 notifyList 一致
type notifyList struct {
	// wait is the ticket number of the next waiter. It is atomically
	// incremented outside the lock.
	//
	// wait是下一个 等待者 (被阻塞的G) 的票号。 它在锁之外以原子方式递增。   (票号从 0 开始)
	wait uint32

	// notify is the ticket number of the next waiter to be notified. It can
	// be read outside the lock, but is only written to with lock held.
	//
	// Both wait & notify can wrap around, and such cases will be correctly
	// handled as long as their "unwrapped" difference is bounded by 2^31.
	// For this not to be the case, we'd need to have 2^31+ goroutines
	// blocked on the same condvar, which is currently not possible.
	//
	//
	// notify  是下一个要通知的 等待者 (被阻塞的G)的票号。
	// 			可以在锁之外读取它，但是只能在 持有锁的情况下写入 (写互斥)
	//
	//	等待   和   通知   都可以绕回，只要“展开”的差异以  2^31   为界，此类情况将得到正确处理。
	//	要避免这种情况，我们需要在同一个  condvar (该sync.Cond) 上阻塞 2^31+ 个 G，这目前是不可能的。
	notify uint32

	// List of parked waiters.     下面的 等待者 (被阻塞的G) 队列的 收尾指针
	lock mutex
	head *sudog
	tail *sudog
}

// less checks if a < b, considering a & b running counts that may overflow the
// 32-bit range, and that their "unwrapped" difference is always less than 2^31.
//
// `less()` 检查是否满足: a < b.  考虑 `a & b`  的运行计数可能溢出32位范围，并且检查它们的 “未包装” 差值始终小于 2 ^ 31
func less(a, b uint32) bool {
	return int32(a-b) < 0
}

// todo runtime.go 的 sunc.runtime_notifyListAdd() 的真正实现
//
// sync.Cond 的方法, 将当前G 加入等待队列
//
// notifyListAdd adds the caller to a notify list such that it can receive
// notifications. The caller must eventually call notifyListWait to wait for
// such a notification, passing the returned ticket number.
//
// `notifyListAdd()`  将  调用方G 添加到通知列表中，以便它可以接收通知。 调用者G 必须最终调用 notifyListWait 来等待此类通知，并传递返回  当前被阻塞的G 的票号,
// 						（票号从0开始） +1表示 下一个人的票号, 而不是自己的票号哦
//
//go:linkname notifyListAdd sync.runtime_notifyListAdd
func notifyListAdd(l *notifyList) uint32 {
	// This may be called concurrently, for example, when called from
	// sync.Cond.Wait while holding a RWMutex in read mode.
	//
	// 这可以被同时调用，例如，当从sync.Cond.Wait 中调用时，同时将RWMutex保持在读取模式。
	return atomic.Xadd(&l.wait, 1) - 1   // 生成下一个人的 票号, 并返回自己的 票号
}

// todo runtime.go 的 sync.runtime_notifyListWait() 的真正实现
//
// sync.Cond 的方法, 根据票号t, 将当前G挂起,放入等待队列中
//
// notifyListWait waits for a notification. If one has been sent since
// notifyListAdd was called, it returns immediately. Otherwise, it blocks.
//go:linkname notifyListWait sync.runtime_notifyListWait
func notifyListWait(l *notifyList, t uint32) {
	lock(&l.lock)

	// Return right away if this ticket has already been notified.
	//
	// 如果已通知此票，请立即返回
	//
	// (如果当前 票号 < 下一个要被通知的 票号)  说明, 自已已经被通知过了
	if less(t, l.notify) {
		unlock(&l.lock)
		return
	}

	// Enqueue itself.   自己入队


	s := acquireSudog()  // 分配 sudog
	s.g = getg()
	s.ticket = t
	s.releasetime = 0
	t0 := int64(0)
	if blockprofilerate > 0 {
		t0 = cputicks()
		s.releasetime = -1
	}

	// 不是很懂 ...
	if l.tail == nil {
		l.head = s
	} else {
		l.tail.next = s
	}
	l.tail = s

	// 将 当前goroutine置于 【等待状态】 并解锁 lock
	// 通过调用 goready（gp），可以使goroutine再次可运行
	goparkunlock(&l.lock, waitReasonSyncCondWait, traceEvGoBlockCond, 3)
	if t0 != 0 {
		blockevent(s.releasetime-t0, 2)
	}

	// 释放 sudog
	releaseSudog(s)
}


// todo runtime.go 的 sync.runtime_notifyListNotifyAll() 的真正实现
//
// sync.Cond 的方法, 将当前 wait 队列中所有 被阻塞的G 唤醒
//
// notifyListNotifyAll notifies all entries in the list.
//go:linkname notifyListNotifyAll sync.runtime_notifyListNotifyAll
func notifyListNotifyAll(l *notifyList) {
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock.
	//
	//快速路径：如果自上次通知以来没有新的 被挂起的G，我们根本不需要获取锁
	//
	//  因为 票号是从 0开始的, 我们从边界 0 开始分析, wait中记录的是下一个即将被分配的票号, 而 notify 中记录的是 下一个可以被唤醒的G 的票号,
	//	如果 wait > notify 说明存在 被阻塞的G,   如果 wait  == notify 说明 没有被挂起的G
	if atomic.Load(&l.wait) == atomic.Load(&l.notify) {
		return
	}

	// Pull the list out into a local variable, waiters will be readied
	// outside the lock.    将 wait列表 拉出到 局部变量中，被阻塞的G 将在锁外准备就绪
	lock(&l.lock)
	s := l.head      	// 拉出 队列头的 G
	l.head = nil		// 清空 队列   (因为 取出来的  头 sudog 中有 next 引用的哦)
	l.tail = nil

	// Update the next ticket to be notified. We can set it to the current
	// value of wait because any previous waiters are already in the list
	// or will notice that they have already been notified when trying to
	// add themselves to the list.
	//
	//
	// 更新下一个要通知的 票号。 我们可以将其设置为当前，或者 在尝试将自己添加到列表时会注意到它们已经被通知。
	//
	//  之所以不清空, wait 和 notify 是因为， 不需要清空啊,  新来的就 取后面的票号, 需要被释放的 也是一个道理啊   (类似一个移动窗口 一直往后划)
	atomic.Store(&l.notify, atomic.Load(&l.wait))
	unlock(&l.lock)

	// Go through the local list and ready all waiters.    逐个唤醒目前全部的 G
	for s != nil {
		next := s.next
		s.next = nil
		readyWithTime(s, 4)
		s = next
	}
}


// todo runtime.go 的 sync.runtime_notifyListNotifyOne() 的真正实现
//
// sync.Cond 的方法, 唤醒下一个需要被唤醒的G
//
// notifyListNotifyOne notifies one entry in the list.
//go:linkname notifyListNotifyOne sync.runtime_notifyListNotifyOne
func notifyListNotifyOne(l *notifyList) {
	// Fast-path: if there are no new waiters since the last notification
	// we don't need to acquire the lock at all.
	//
	//快速路径：如果自上次通知以来没有新的 被挂起的G，我们根本不需要获取锁
	//
	//  因为 票号是从 0开始的, 我们从边界 0 开始分析, wait中记录的是下一个即将被分配的票号, 而 notify 中记录的是 下一个可以被唤醒的G 的票号,
	//	如果 wait > notify 说明存在 被阻塞的G,   如果 wait  == notify 说明 没有被挂起的G
	if atomic.Load(&l.wait) == atomic.Load(&l.notify) {
		return
	}

	lock(&l.lock)

	// Re-check under the lock if we need to do anything.   如果需要执行任何操作，请在锁下重新检查
	//
	// 再次做一次  wait  和 notify 的比较判断
	t := l.notify
	if t == atomic.Load(&l.wait) {
		unlock(&l.lock)
		return
	}

	// Update the next notify ticket number.   更新下一个 被唤醒的 票号   todo (从 第一个开始逐个往后唤醒G的)
	atomic.Store(&l.notify, t+1)

	// Try to find the g that needs to be notified.
	// If it hasn't made it to the list yet we won't find it,
	// but it won't park itself once it sees the new notify number.
	//
	// This scan looks linear but essentially always stops quickly.
	// Because g's queue separately from taking numbers,
	// there may be minor reorderings in the list, but we
	// expect the g we're looking for to be near the front.
	// The g has others in front of it on the list only to the
	// extent that it lost the race, so the iteration will not
	// be too long. This applies even when the g is missing:
	// it hasn't yet gotten to sleep and has lost the race to
	// the (few) other g's that we find on the list.
	//
	//
	//	尝试找到需要通知的g
	//		如果尚未进入 wait列表，但我们找不到它，但我们找不到它，但是一旦看到新的 票号，它就不会停放
	//
	// 此扫描看起来是线性的，但实际上总是很快停止
	// 因为g的队列与取数字的队列分开，所以列表中可能会有较小的重新排序，但是我们希望我们要寻找的g在前面
	//
	// G 仅在失去竞争的情况下才在列表中位于 G的前面，因此迭代的时间不会太长。 即使 g丢失，这也适用：它尚未入睡，已经失去了 与 列表中其他 g的竞争
	//
	//
	//  逐个将 wait 队列中的 G 遍历, 直到 拿到 当前票号对应的G,  唤醒该G
	for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {

		// 逐个的和 当前需要被释放的 票号对比
		if s.ticket == t {
			n := s.next
			if p != nil {
				p.next = n
			} else {
				l.head = n
			}
			if n == nil {
				l.tail = p
			}
			unlock(&l.lock)
			s.next = nil
			readyWithTime(s, 4)  // 唤醒该 G
			return
		}
	}
	unlock(&l.lock)
}

// todo runtime.go 的 sync.runtime_notifyListCheck() 的真正实现
//
// 用来检验 runtime.go 中的 notifyList 结构体 和 当前sema.go 的 notifyList结构体 是否 结构和对齐 一致
//
//go:linkname notifyListCheck sync.runtime_notifyListCheck
func notifyListCheck(sz uintptr) {
	if sz != unsafe.Sizeof(notifyList{}) {
		print("runtime: bad notifyList size - sync=", sz, " runtime=", unsafe.Sizeof(notifyList{}), "\n")
		throw("bad notifyList size")
	}
}

// todo 获取系统纳秒时间戳
//go:linkname sync_nanotime sync.runtime_nanotime
func sync_nanotime() int64 {
	return nanotime()
}
