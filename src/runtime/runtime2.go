// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// defined constants
const (

	//	todo G 的状态值枚举定义
	//
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack (and hence its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	//
	// TODO(austin): The _Gscan bit could be much lighter-weight.
	// For example, we could choose not to run _Gscanrunnable
	// goroutines found in the run queue, rather than CAS-looping
	// until they become _Grunnable. And transitions like
	// _Gscanwaiting -> _Gscanrunnable are actually okay because
	// they don't affect stack ownership.

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	_Grunnable // 1

	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (g.m and g.m.p are valid).
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.g., a channel wait
	// queue) so it can be ready()d when necessary. The stack is
	// not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a stack
	// allocated. The G and its stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	_Gcopystack // 8

	// _Gpreempted means this goroutine stopped itself for a
	// suspendG preemption. It is like _Gwaiting, but nothing is
	// yet responsible for ready()ing it. Some suspendG must CAS
	// the status to _Gwaiting to take responsibility for
	// ready()ing this G.
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (


	// todo P 的状态值枚举定义
	//
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. Typically, it's on the idle P list and available
	// to the scheduler, but it may just be transitioning between
	// other states.
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty.
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.g., to schedule a locked G).
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity to an M in a syscall but is not owned by it and
	// may be stolen by another M. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P. Note that there's an ABA hazard: even if
	// an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim.
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.g., trace buffers).
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
//
// todo 锁 (信号灯的底层实现)
//
// 	互斥锁的真正底层实现:  在无竞争的情况下，其速度与自旋锁一样快（只有一些用户级指令），但是在争用路径上它们却在内核中休眠。
// 	置零的互斥锁已解锁（无需初始化每个锁）。
type mutex struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	//
	//	基于Futex 的impl将其视为uint32 key，而将基于sema的impl视为 `M* waitm`
	//	曾经是一个联合，但是联合破坏了精确的GC。
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// todo func类型定义
type funcval struct {
	fn uintptr   // 指向 func 地址 的值
	// variable-size, fn-specific data here
}


/**
iface  和 eface 分别是 interface 类型的两种定义

Go语言中，每个变量都有唯一个【静态类型】，这个类型是编译阶段就可以确定的。有的变量可能除了静态类型之外，还会有【动态混合类型】.

interface的“类型”和“数据值”是在“一起的”，而 reflect 的“类型”和“数据值”是分开的

 todo interface源码(位于”Go SDK/src/runtime/runtime2.go“)中的 eface和 iface 会
 todo	和 反射源码(位于”GO SDK/src/reflect/value.go“)中的emptyInterface和nonEmptyInterface保持数据同步！
 */

// 非 interface{} 类型接口  (iface表示的是包含方法的interface)
type iface struct {
	tab  *itab				// 决定 类型的
	data unsafe.Pointer		// 指向具体的数据的指针
}

// interface{} 类型接口  (不包含方法的interface)
type eface struct {
	// _type可以认为是Go语言中所有类型的公共描述，Go语言中几乎所有的数据结构都可以抽象成_type，是所有类型的表现，可以说是万能类型   todo (看 rtype)
	_type *_type    		// 用于 指向赋值给接口的具体类型  (动态类型的具体类型)
	data  unsafe.Pointer  	// 指向具体数据的指针
}

func efaceOf(ep *interface{}) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
// 1. Never hold an muintptr locally across a safe point.
//
// 2. Any muintptr in the heap must be owned by the M itself so it can
//    ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}


// todo 用于保存G切换时上下文的缓存结构体
type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	sp   uintptr  			// 当前的栈指针
	pc   uintptr			// 计数器
	g    guintptr			// g 自身 地址
	ctxt unsafe.Pointer 	// 这必须是一个指针，以便gc对其进行扫描
	ret  sys.Uintreg		// g 的 return 值 ??
	lr   uintptr
	bp   uintptr // for GOEXPERIMENT=framepointer
}

/**
 sudog 在等待列表中表示g，例如用于在“通道”上发送/接收。

 sudog 是必需的，因为g ↔ 同步对象 关系是 多对多的。
	一个g 可以出现在 许多等待列表上， 因此一个g可能有许多sudog。
	并且许多 g 可能正在 等待同一个 同步对象，因此 一个对象可能有许多sudog

 sudog 是从特殊池中分配的。 使用 `acquireSudog` 和 `releaseSudog` 分配 和 释放 它们
 */
// sudog represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
//
// todo 被阻塞的goroutine将会封装成sudog，加入到channel的等待队列中
//
//  todo 什么是 sudog
//
// todo sudog 代表在 wait列表 里的 g，比如向 channel 发送/接收内容时
//
// sudog 是必需的，因为 `g <--> snyc.Obj` 关系是多对多的.

// 		一个 g 可以出现在许多 wait列表 上，因此一个g可能有许多 sudog.
//
// 		并且许多 g 可能正在 等待 同一个 sunc.Obj，因此一个 sync.Obj 可能有许多 sudog
//
// todo sudog 是从一个特殊的池中进行分配的。用 acquireSudog 和 releaseSudog 来分配 和 释放 sudog
type sudog struct {

	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.
	//
	// 以下字段受此 sudog 阻塞的 chan 的 hchan.lock保护. 对于参与 chan 操作的sudog，srinkstack 依赖于此

	// 指向的  g
	g *g

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	//
	//`isSelect`表示g正在参加`select`，因此必须将“ g.selectDone`设为CAS”才能赢得唤醒比赛。
	// todo 当前 G 是否在参与 select 关键字的 逻辑处理中  (参照 select.go 中的 select源码)
	isSelect bool

	// 如果实在 队列结构中使用, 那么就有这两个
	next     *sudog
	prev     *sudog
	elem     unsafe.Pointer // data element (may point to stack)    数据元素（可能指向 栈）   todo  在 和 chan 的操作中迭代的 数据 ????  sudog.elem 是指向 接收目标的内存的指针

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64
	releasetime int64

	// 票号, 在 sync.Cond 中被使用的东西， 在 sema 信号灯中也用到了
	ticket      uint32
	parent      *sudog // semaRoot binary tree
	waitlink    *sudog // g.waiting list or semaRoot
	waittail    *sudog // semaRoot

	// 被阻塞的 sudog 所属的 chan
	c           *hchan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr
	err  uintptr // error number
}

// describes how to handle callback
type wincallbackcontext struct {
	gobody       unsafe.Pointer // go function to call
	argsize      uintptr        // callback arguments size (in bytes)
	restorestack uintptr        // adjust stack on return by (in bytes) (386 only)
	cleanstack   bool
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
type stack struct {
	lo uintptr
	hi uintptr
}

/**
可以看到如果 G 需要等待资源时,
	会记录G的 运行状态 到 g.sched, 然后把状态改为等待中(_Gwaiting), 再让当前的 M 继续运行其他G.
	todo 等待中的G保存在哪里, 什么时候恢复 是 【等待的资源决定的】, 比如： 对channel的等待会让G放到channel中的链表.
 */

// todo G 的定义
type g struct {	// todo G 中有 M
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	//
	//
	// g 使用的 栈空间 (lo - hi)
	stack       stack   // offset known to runtime/cgo

	// todo G 中的两个重要的 栈空间
	// 检查栈空间是否足够的值, 低于这个值会扩张栈, 0 是 go代码使用的   (在 当前G 被抢占时, 该值会被赋予一个巨大的值 `stackPreempt`)
	stackguard0 uintptr // offset known to liblink
	// 检查栈空间是否足够的值, 低于这个值会扩张栈, 1 是 原生代码使用的 (系统调用使用)
	stackguard1 uintptr // offset known to liblink

	_panic       *_panic // innermost panic - offset known to liblink

	// 这个是当前 goroutine 的 defer 链表的首个 defer实例
	_defer       *_defer // innermost defer 最内在的延迟

	// 运行当前 G 被用到的 M  todo G 里面 有 M
	m            *m      // current m; offset known to arm liblink

	// g 的调度数据, 当g中断时会保存当前的 pc 和 rsp 等值到这里, 恢复运行时会使用这里的值
	sched        gobuf
	syscallsp    uintptr        // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc    uintptr        // if status==Gsyscall, syscallpc = sched.pc to use during gc
	stktopsp     uintptr        // expected sp at top of stack, to check in traceback

	// 当G 被这里会有值, 唤醒时传递的参数
	param        unsafe.Pointer // passed parameter on wakeup

	// g的当前状态 (看 G 状态的枚举定义咯)
	atomicstatus uint32
	stackLock    uint32 // sigprof/scang lock; TODO: fold in to atomicstatus
	goid         int64

	// 下一个g, 当g在链表结构中会使用  (当 G 处于 P的runq 或者 P的gFree 或者 allgs 或者 schedt的runq 或者 schedt的gFree 队列中时, G会以链表的形式指向下一个G)
	schedlink    guintptr
	waitsince    int64      // approx time when the g become blocked

	// G被 挂起时 的原因
	waitreason   waitReason // if status==Gwaiting

	// g 是否被抢占中
	preempt       bool // preemption signal, duplicates stackguard0 = stackpreempt
	preemptStop   bool // transition to _Gpreempted on preemption; otherwise, just deschedule
	preemptShrink bool // shrink stack at synchronous safe point

	// asyncSafePoint is set if g is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	asyncSafePoint bool

	paniconfault bool // panic (instead of crash) on unexpected fault address
	gcscandone   bool // g has scanned stack; protected by _Gscan bit in status
	throwsplit   bool // must not split stack
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	activeStackChans bool

	raceignore     int8     // ignore race detection events
	sysblocktraced bool     // StartTrace has emitted EvGoInSyscall about this goroutine
	sysexitticks   int64    // cputicks when syscall has returned (for tracing)
	traceseq       uint64   // trace event sequencer
	tracelastp     puintptr // last P emitted an event for this goroutine

	// g 是否要求要回到这个M执行, 有的时候g中断了恢复会要求使用原来的M执行 todo (表示 当前 G 锁定了 M 的地址, 当前G出自某原因中断了, 下次 当前 G 恢复时, 有用该 M 来执行 当前G)
	lockedm        muintptr
	sig            uint32
	writebuf       []byte
	sigcode0       uintptr
	sigcode1       uintptr
	sigpc          uintptr
	gopc           uintptr         // pc of go statement that created this goroutine
	ancestors      *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	startpc        uintptr         // pc of goroutine function
	racectx        uintptr

	// 当前 G 参与 chan 操作被阻塞时, 被封装成的sudog 的引用
	waiting        *sudog         // sudog structures this g is waiting on (that have a valid elem ptr); in lock order   这个g正在等待的sudog结构（具有有效的elem ptr）； 锁定顺序
	cgoCtxt        []uintptr      // cgo traceback context
	labels         unsafe.Pointer // profiler labels
	timer          *timer         // cached timer for time.Sleep   todo 缓存 time.Sleep() 语句的 定时器
	selectDone     uint32         // are we participating in a select and did someone win the race?

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	gcAssistBytes int64
}


// todo M 的定义
type m struct {  // todo M 里面 有 P 和 G

	// 用于调度的特殊g, `调度` 和 `执行系统调用` 时会切换到这个g 【系统G】
	//
	// 每个M启动都有一个叫 g0 的系统堆栈，
	// 		runtime通常使用systemstack、mcall 或asmcgocall 临时切换到系统堆栈，以执行必须不被抢占的任务、不得增加用户堆栈的任务或切换用户goroutines
	// 		在系统堆栈上运行的代码 【隐式不可抢占】，【垃圾收集器不扫描系统堆栈】。
	// 		在系统堆栈上运行时，不会使用当前用户堆栈执行。
	//
	// g0 是仅用于 【负责调度】 的G, g0  不指向任何可执行的函数, 每个m都会有一个自己的g0,
	//    在 【调度】 或 【系统调用】 时会使用g0的栈空间, 【全局变量的g0】 是m0的g0  (m0 是 main 线程)
	//
	// m0 是启动程序后的主线程,   这个m对应的实例会在 全局变量m0中, 不需要在heap上分配,
	//    m0负责执行  初始化操作  和  启动第一个g, 在之后m0就和其他的m一样了.
	//
	//
	g0      *g     // goroutine with scheduling stack
	morebuf gobuf  // gobuf arg to morestack
	divmod  uint32 // div/mod denominator for arm - known to liblink

	// Fields not known to debuggers.
	procid        uint64       // for debuggers, but offset not hard-coded
	gsignal       *g           // signal-handling g
	goSigStack    gsignalStack // Go-allocated signal handling stack
	sigmask       sigset       // storage for saved signal mask

	// todo 很重要的
	// 线程本地存储（用于x86 extern寄存器）
	//
	// TLS 的全称是Thread-local storage, 代表每个线程的中的本地数据.
	//   	例如标准c中的errno就是一个典型的TLS变量, 每个线程都有一个独自的errno, 写入它不会干扰到其他线程中的值.
	//   	go在实现协程时非常依赖TLS机制, 会用于获取系统线程中当前的G和G所属的M的实例.
	//
	//   	因为go并不使用glibc, 操作TLS会使用系统原生的接口, 以linux x64为例,
	//   	go在新建M时会调用arch_prctl这个syscall设置FS寄存器的值为M.tls的地址,
	//   	运行中每个M的FS寄存器都会指向它们对应的M实例的tls, linux内核调度线程时FS寄存器会跟着线程一起切换,
	//   	这样go代码只需要访问FS寄存器就可以存取线程本地的数据.
	//
	tls           [6]uintptr   // thread-local storage (for x86 extern register)

	// todo 很重要的 一个函数指针变量
	mstartfn      func()

	// todo 当前 M 正在运行的 g
	curg          *g       // current running goroutine
	caughtsig     guintptr // goroutine running during fatal signal

	// 当前 M 正在拥有的 P (当 M使用G执行 `系统调用`  或者 M 处于空闲时, p的值为 `nil`)
	p             puintptr // attached p for executing go code (nil if not executing go code)

	// 指向 下一个 潜在关联性的 P 地址 (当 M 被 重新启用时, 会先去尝试和 这个 P 关联)
	nextp         puintptr
	oldp          puintptr // the p that was attached before executing a syscall
	id            int64
	mallocing     int32
	throwing      int32

	// 满足 if != "" 时, 继续将 之前的 G 运行在 当前 M 上      ( 调用 `stopTheWorld()` 时, 该值 会写上 Stop 的 原因 )
	preemptoff    string // if != "", keep curg running on this m

	// 用来记录当前 M 被抢占的 计数 (多少个g抢占M的计数????)
	locks         int32
	dying         int32
	profilehz     int32

	// 当前 M 是否在做 【自旋】
	spinning      bool // m is out of work and is actively looking for work
	blocked       bool // m is blocked on a note
	newSigstack   bool // minit on C thread called sigaltstack
	printlock     int8
	incgo         bool   // m is executing a cgo call
	freeWait      uint32 // if == 0, safe to free g0 and delete m (atomic)

	// 用来 实现随机函数 存放的两个 seed
	fastrand      [2]uint32
	needextram    bool
	traceback     uint8
	ncgocall      uint64      // number of cgo calls in total
	ncgo          int32       // number of cgo calls currently in progress
	cgoCallersUse uint32      // if non-zero, cgoCallers in use temporarily
	cgoCallers    *cgoCallers // cgo traceback if crashing in cgo call

	// M休眠时 使用的信号量, 唤醒M时会通过它唤醒
	park          note
	alllink       *m // on allm

	// 下一个m, 当m在链表结构中会使用  (参照 G 定义中 schedlink 的定义自明)
	schedlink     muintptr

	// 分配内存时使用的本地分配器, todo  和p.mcache一样(拥有P时会复制过来)
	mcache        *mcache

	// 当 M 被恢复时, 可能使用该 G 继续执行,  和 G 中定义的  lockedm 相呼应  (一般来说只有 GC 的时候 这个才有值)
	lockedg       guintptr
	createstack   [32]uintptr // stack that created this thread.
	lockedExt     uint32      // tracking for external LockOSThread
	lockedInt     uint32      // tracking for internal lockOSThread
	nextwaitm     muintptr    // next m waiting for lock

	//  解锁函数 指针, 这里的解锁函数会对 解除channel.lock的锁定
	waitunlockf   func(*g, unsafe.Pointer) bool

	// 等待 锁变量的指针,  由 waitunlockf 回调函数使用
	waitlock      unsafe.Pointer
	waittraceev   byte
	waittraceskip int
	startingtrace bool
	syscalltick   uint32
	freelink      *m // on sched.freem

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	libcall   libcall
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // stores syscall parameters on windows

	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails. Accessed atomically.
	preemptGen uint32

	// Whether this is a pending preemption signal on this M.
	// Accessed atomically.
	signalPending uint32

	dlogPerM

	mOS
}

// todo P 的定义
type p struct {  // todo P 中有 M
	id          int32

	// P 的状态枚举值 (参照 P 状态自枚举定义)
	status      uint32 // one of pidle/prunning/...

	// 下一个p, 当p在链表结构中会使用
	link        puintptr

	// 增加P中记录的调度次数( 每61次优先获取一次 schedt的全局 runq队列中的G)
	schedtick   uint32     // incremented on every scheduler call
	syscalltick uint32     // incremented on every system call
	sysmontick  sysmontick // last tick observed by sysmon

	// todo P 中有 M
	// 拥有这个P的M (当 P 空闲时, 该值为 `nil`)
	m           muintptr   // back-link to associated m (nil if idle)

	// 分配内存时使用的本地分配器 todo (P 和 M 关联时, 会复制给 M, 查看 M 定义自明)
	//
	// P 是一个虚拟的资源, 同一时间只能有一个线程访问同一个P, 所以P中的数据不需要锁.
	//   为了分配对象时有更好的性能, 各个P中都有span的缓存(也叫mcache)
	mcache      *mcache  // 各个P中按span类型的不同, 有67*2=134个span的缓存,
	pcache      pageCache
	raceprocctx uintptr

	deferpool    [5][]*_defer // pool of available defer structs of different sizes (see panic.go)
	deferpoolbuf [5][32]*_defer

	// Cache of goroutine ids, amortizes accesses to runtime·sched.goidgen.
	goidcache    uint64
	goidcacheend uint64

	// Queue of runnable goroutines. Accessed without lock.   可运行goroutine队列。 无锁访问。 （即: 锁自由）
	//
	// P中 runq的头  todo 本地运行队列的出队序号 (因为 runq队列一直在变)
	runqhead uint32
	// P中 runq的尾 todo 本地运行队列的入队序号 (因为 runq队列一直在变)
	runqtail uint32

	// todo  P 中可运行的 G 队列
	runq     [256]guintptr

	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	//
	// `runnext`：
	// 		如果非nil，则是一个可运行的`G`，已经由当前的G`准备好了，如果运行的G中还有剩余时间，则应该运行而不是runq中的 剩余 G的时间片。
	// 		它将继承当前时间片中剩余的时间。 如果将一组“ goroutines”锁定在“通信等待”模式中，
	// 		则此“ schedules”将被设置为一个单元，并消除了（潜在的）较大的调度延迟，
	// 		否则会因将已准备好的“ goroutines”添加到“调度”而产生。 运行队列的末尾。
	//
	//  todo 这个放置 一个下一个 可以运行的G ??
	runnext guintptr

	// Available G's (status == Gdead)
	// todo P 中的 自由 G 队列 (状态为: Gdead 的 G)
	gFree struct {
		gList		// 队列 头指针
		n int32  	// 个数
	}

	sudogcache []*sudog		// 重复利用的 sodug 壳 缓存
	sudogbuf   [128]*sudog

	// Cache of mspan objects from the heap.
	mspancache struct {
		// We need an explicit length here because this field is used
		// in allocation codepaths where write barriers are not allowed,
		// and eliminating the write barrier/keeping it eliminated from
		// slice updates is tricky, moreso than just managing the length
		// ourselves.
		len int
		buf [128]*mspan
	}

	tracebuf traceBufPtr

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	traceSweep bool
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	traceSwept, traceReclaimed uintptr

	palloc persistentAlloc // per-P to avoid mutex

	_ uint32 // Alignment for atomic fields below

	// The when field of the first entry on the timer heap.
	// This is updated using atomic functions.
	// This is 0 if the timer heap is empty.
	//
	//
	//	记录 timer 堆上的 第一个 timer 的 定时时间点  todo (因为  马上当前 P 就要 运行 该 timer 了)
	//	这是使用原子函数更新的。
	//	如果计时器堆为空，则为0
	timer0When uint64

	// Per-P GC state
	gcAssistTime         int64    // Nanoseconds in assistAlloc
	gcFractionalMarkTime int64    // Nanoseconds in fractional mark worker (atomic)

	// todo 后台GC的worker函数, 如果它存在M会优先执行它
	gcBgMarkWorker       guintptr // (atomic)
	gcMarkWorkerMode     gcMarkWorkerMode

	// gcMarkWorkerStartTime is the nanotime() at which this mark
	// worker started.
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	//
	// GC 的本地工作队列
	// gcw是 当前P的GC工作缓冲区高速缓存。工作缓冲区由写屏障填充，由辅助更改器耗尽，并放置在某些GC状态转换上。
	gcw gcWork

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	wbBuf wbBuf    // 这个是 P 的 GC 写屏障 缓存队列

	runSafePointFn uint32 // if 1, run sched.safePointFn at next safe point

	// Lock for timers. We normally access the timers while running
	// on this P, but the scheduler can also do it from a different P.
	//
	// 锁定计时器     我们通常在此P上运行时访问计时器，但调度程序也可以从其他P上执行此操作
	//
	// 使用当前 P 操作 timer 定时器任务时的 锁
	timersLock mutex

	// Actions to take at some time. This is used to implement the
	// standard library's time package.
	// Must hold timersLock to access.
	//
	//	有时需要采取的行动。 这用于实现标准库的时间包。
	//	必须持有timersLock才能访问。
	timers []*timer   //  todo 分散在 每个 P 中的 timer 或者 ticker 定时器实例  (runtimeTimer 堆的底层数组)

	// Number of timers in P's heap.
	// Modified using atomic instructions.
	//
	// 记录当前 P 的 timer 堆上的 timer 个数
	// 原子操作
	numTimers uint32

	// Number of timerModifiedEarlier timers on P's heap.
	// This should only be modified while holding timersLock,
	// or while the timer status is in a transient state
	// such as timerModifying.
	//
	//
	// P 的 timer 堆上的 状态为: `timerModifiedEarlier timer数量
	//  仅应在保持 timersLock 或 计时器状态为 【过渡状态】（例如timerModifying）时进行修改
	adjustTimers uint32

	// Number of timerDeleted timers in P's heap.
	// Modified using atomic instructions.
	//
	// 当前 P 的 timer 堆中  状态为 【可被删除】 的 timer的个数
	// 原子操作
	deletedTimers uint32

	// Race context used while executing timer functions.
	timerRaceCtx uintptr

	// preempt is set to indicate that this P should be enter the
	// scheduler ASAP (regardless of what G is running on it).
	preempt bool

	pad cpu.CacheLinePad   // 做对齐用
}

// todo 系统调度器 的定义
//		全局调度器，全局只有一个schedt类型的实例
type schedt struct {
	// accessed atomically. keep at top to ensure alignment on 32-bit systems.
	// 下面三个变量需以原子访问访问。保持在 struct 顶部，确保其在 32 位系统上可以对齐
	goidgen   uint64
	lastpoll  uint64 // time of last network poll, 0 if currently polling
	pollUntil uint64 // time to which current poll is sleeping


	// 调度器中的锁
	lock mutex

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().  当增加 nmidle，nmidlelocked，nmsys，nmfreed时，请确保调用 checkdead()
	//
	// todo 调度器的 【空闲 M 队列】 只存放目前没有被使用 (空闲) 的 M
	midle        muintptr // idle m's waiting for work

	nmidle       int32    // number of idle m's waiting for work							当前等待工作的空闲 m 计数
	nmidlelocked int32    // number of locked m's waiting for work							当前等待工作的被 lock 的 m 计数
	mnext        int64    // number of m's that have been created and next M ID				已创建的m个数和下一个M ID
	maxmcount    int32    // maximum number of m's allowed (or die)							m允许（或死亡）的最大数目
	nmsys        int32    // number of system m's not counted for deadlock					系统m的数量不计为死锁
	nmfreed      int64    // cumulative number of freed m's									释放的m的累计数量

	ngsys uint32 // number of system goroutines; updated atomically							系统goroutine的数量; 原子更新

	// todo 调度器的 【空闲 P】 只存放 Pidle 状态的 P  （注意: Pdead 的P 只会被 GC 掉）
	pidle      puintptr // idle p's
	npidle     uint32

	// 正在做 自旋的 M 的个数
	nmspinning uint32 // See "Worker thread parking/unparking" comment in proc.go.

	// Global runnable queue.
	// todo 存放 可运行的 G 队列     状态 Grunnable 的 G
	//  			从 `Gsyscall` 转出来的G，都会被放置到调度器的可运行G队列中
	runq     gQueue
	// 可运行 G 队列的 长度
	runqsize int32

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	disable struct {
		// user disables scheduling of user goroutines.
		user     bool
		runnable gQueue // pending runnable Gs
		n        int32  // length of runnable
	}

	// Global cache of dead G's.
	// todo 存放 自由 G 的队列,  状态 Gdead 的 G （注意: 不是 Gidle 的哦, 这种是新创建的G, 被放到 全局 G 队列）
	gFree struct {
		lock    mutex
		stack   gList // Gs with stacks				栈中的 free g 队列
		noStack gList // Gs without stacks			堆中的 free g 队列
		n       int32
	}

	// Central cache of sudog structs. sudog结构的中央缓存
	// sudog 结构的集中缓存
	sudoglock  mutex
	sudogcache *sudog

	// Central pool of available defer structs of different sizes.   不同大小的可用 defer 结构的中央池
	// 不同大小的可用的 defer struct 的集中缓存池
	deferlock mutex
	deferpool [5]*_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	freem *m

	/**
	下面 5 个字段是  辅助 GC 执行而存在的
	 */
	// 值是 1 时, 说明正在 GC, 然后调度器 停止 P 和 M
	gcwaiting  uint32 	// gc is waiting to run     gc 等待运行状态.   (作为gc任务被执行期间的辅助标记、停止计数和通知机制)
	// 当该值 为 0 时, 说明调度器 完全停止工作, 这是 GC就可以开始了
	stopwait   int32	// 用来对 还未被停止调度的 P 进行计数 (GC到来时)
	stopnote   note		// 用来向 垃圾回收器 告知 调度工作已经完全停止的 通知机制  (信号量, 用来阻塞自身 <阻塞GC> 等待 调度器完全停止 方可开始 GC)
	// 值为 1 时, 表示 暂停 系统监控任务 (只有 gc 时才会这么做)
	sysmonwait uint32 	// 作为 系统检测任务被执行期间的停止计数和通知机制    (系统监控任务是一个 死循环, 只有垃圾回收时, 才会暂停)
	sysmonnote note		// 用来 向执行系统 检测任务的程序 发送通知		(信号量, 用来阻塞自身 <阻塞系统监控> 等待 GC结束)

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	//
	// 应在下一个GC上的每个P上调用safepointFn
	// 如果设置了p.runSafePointFn，则为safepoint
	safePointFn   func(*p)
	safePointWait int32
	safePointNote note

	profilehz int32 // cpu profiling rate 	CPU分析率

	procresizetime int64 // nanotime() of last change to gomaxprocs			上次修改 GOMAXPROCS 的纳秒时间
	totaltime      int64 // ∫gomaxprocs dt up to procresizetime
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // add SA_ONSTACK to libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
type _func struct {
	entry   uintptr // start pc
	nameoff int32   // function name

	args        int32  // in/out args size
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

	pcsp      int32
	pcfile    int32
	pcln      int32
	npcdata   int32
	funcID    funcID  // set for certain special runtime functions
	_         [2]int8 // unused
	nfuncdata uint8   // must be last
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
type funcinl struct {
	zero  uintptr // set to 0 to distinguish from _func
	entry uintptr // entry of the real (the "outermost") frame.
	name  string
	file  string
	line  int
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory
// Needs to be in sync with
// ../cmd/compile/internal/gc/reflect.go:/^func.dumptabs.
//
//
// 编译器已知的Itab布局
// 在 非GC内存 中分配
// 需要与
// ../cmd/compile/internal/gc/reflect.go:/^func.dumptabs
//
//
type itab struct {

	// todo  主要包含 两部分:
	//
	// 		一部分是  	确定唯一的包含方法的interface的具体结构类型
	//		一部分是	指向具体方法集的指针

	// inter 和 _type 共同决定了 interface的具体结构类型
	inter *interfacetype			// 用于 指向 接口本身类型
	_type *_type					// 用于 指向 实现了接口的 具体类型  (动态类型的具体类型)

	// _type.hash 的拷贝, 用于 查询 和 类型判断
	hash  uint32 // copy of _type.hash. Used for type switches.

	// 做内存对齐用
	_     [4]byte

	// 指向 具体方法集的指针
	fun   [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

// Lock-free stack node.
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle uint32
}

// startup_random_data holds random bytes initialized at startup. These come from
// the ELF AT_RANDOM auxiliary vector (vdso_linux_amd64.go or os_linux_386.go).
var startupRandomData []byte

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < sys.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

/**
todo 这个是 defer 关键字 的定义



// _defer在延迟调用列表中保留一个条目。
//如果在此处添加字段，请添加代码以在freedefer和deferProcStack中将其清除
//此结构必须与cmd / compile / internal / gc / reflect.go：deferstruct中的代码匹配
//和cmd / compile / internal / gc / ssa.go：（* state）.call。
//一些延迟将分配在堆栈上，而一些延迟将分配在堆上。
//在逻辑上，所有延迟器都是堆栈的一部分，因此请为
//不需要初始化它们。 所有延迟都必须手动扫描，
//并标记堆延迟。
 */
// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in freedefer and deferProcStack
// This struct must match the code in cmd/compile/internal/gc/reflect.go:deferstruct
// and cmd/compile/internal/gc/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
type _defer struct {
	siz     int32 // includes both arguments and results
	started bool
	heap    bool
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	openDefer bool

	// sp 和 pc 都是 寄存器

	// 函数栈指针  sp
	sp        uintptr  // sp at time of defer
	// 程序计数器  pc
	pc        uintptr  // pc at time of defer

	// defer  func () 对应的 函数引用  (函数地址)
	fn        *funcval // can be nil for open-coded defers
	_panic    *_panic  // panic that is running defer

	// 指向自身结构的指针，用于链接多个defer todo 就是用这个实现了 类似 多个defer 入栈的结构 ??
	link      *_defer

	// If openDefer is true, the fields below record values about the stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // funcdata for the function associated with the frame
	varp uintptr        // value of varp for the stack frame
	// framepc is the current pc associated with the stack frame. Together,
	// with sp above (which is the sp associated with the stack frame),
	// framepc/sp can be used as pc/sp pair to continue a stack trace via
	// gentraceback().
	framepc uintptr
}


// todo panic 关键字的实现
//
// A _panic holds information about an active panic.
//
// This is marked go:notinheap because _panic values must only ever
// live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
//
//go:notinheap
type _panic struct {
	// 指向 defer 调用时参数的指针
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink   指向在紧急情况下运行的延迟调用参数的指针； 无法移动-已知为liblink
	// 调用 panic 时传入的参数
	arg       interface{}    // argument to panic
	// 	指向了更早调用的 runtime._panic 结构
	link      *_panic        // link to earlier panic
	pc        uintptr        // where to return to in runtime if this panic is bypassed     	如果忽略了此紧急情况，将在运行时返回哪里
	sp        unsafe.Pointer // where to return to in runtime if this panic is bypassed			如果忽略了此紧急情况，将在运行时返回哪里
	recovered bool           // whether this panic is over										panic是否结束
	aborted   bool           // the panic was aborted											恐慌中止了
	goexit    bool

	// pc、sp 和 goexit 三个字段都是为了修复 `runtime.Goexit()` 的问题引入的
	// 		`runtime.Goexit()` 函数能够 只结束调用该函数的 Goroutine 而 不影响其他的 Goroutine，
	// 		但是该函数会被 defer 中的 panic 和 recover 取消，引入这三个字段的目的就是为了解决这个问题.
}

// stack traces
type stkframe struct {
	fn       funcInfo   // function being run
	pc       uintptr    // program counter within fn
	continpc uintptr    // program counter where execution can continue, or 0 if not
	lr       uintptr    // program counter at caller aka link register
	sp       uintptr    // stack pointer at pc
	fp       uintptr    // stack pointer at caller aka frame pointer
	varp     uintptr    // top of local variables
	argp     uintptr    // pointer to function arguments
	arglen   uintptr    // number of bytes at argp
	argmap   *bitvector // force use of this argmap
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid int64     // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGGIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonTimerGoroutineIdle                      // "timer goroutine (idle)"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonPreempted                               // "preempted"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGGIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonTimerGoroutineIdle:    "timer goroutine (idle)",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonPreempted:             "preempted",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

// todo  三个全局的列表主要为了统计运行时系统的的所有G、M、P
var (
	//  全局 g 队列中 g 的个数  （全局 G 队列 被定义在 proc.go 中）
	allglen    uintptr
	// runtime 的 全局 M 队列 (存放所有 M)  todo M在被创建之初会被加入到全局的M列表
	// 					全局M列表的作用是运行时系统在需要的时候会通过它获取到所有的M的信息，同时防止M被gc
	allm       *m

	// runtime 的 全局 P 队列 (存放所有 P)  todo 在根据 GOMAXPROCS 确认P的最大数量后，运行时系统会根据这个数值初始化全局的P列表
	allp       []*p  // len(allp) == gomaxprocs; may change at safe points, otherwise immutable      len(allp) == GOMAXPROCS 设定的值, 可能会在安全点发生变化，否则将保持不变
	allpLock   mutex // Protects P-less reads of allp and all writes   保护allp的无P读取和所有写入
	gomaxprocs int32
	ncpu       int32
	forcegc    forcegcstate

	// 系统 调度器 实例
	sched      schedt
	newprocs   int32

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// Set on startup in asm_{386,amd64}.s
	processorVersionInfo uint32
	isIntel              bool
	lfenceBeforeRdtsc    bool

	goarm                uint8 // set by cmd/link on arm systems
	framepointer_enabled bool  // set by cmd/link
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)
