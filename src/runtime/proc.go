// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

var buildVersion = sys.TheVersion

// set using cmd/go/internal/modload.ModInfoProg
var modinfo string

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// Design doc at https://golang.org/s/go11sched.

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
//
// The current approach:
// We unpark an additional thread when we ready a goroutine if (1) there is an
// idle P and there are no "spinning" worker threads. A worker thread is considered
// spinning if it is out of local work and did not find work in global run queue/
// netpoller; the spinning state is denoted in m.spinning and in sched.nmspinning.
// Threads unparked this way are also considered spinning; we don't do goroutine
// handoff so such threads are out of work initially. Spinning threads do some
// spinning looking for work in per-P run queues before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning state
// and then parks.
// If there is at least one spinning thread (sched.nmspinning>1), we don't unpark
// new threads when readying goroutines. To compensate for that, if the last spinning
// thread finds work and stops spinning, it must unpark a new spinning thread.
// This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism utilization.
//
// The main implementation complication is that we need to be very careful during
// spinning->non-spinning thread transition. This transition can race with submission
// of a new goroutine, and either one part or another needs to unpark another worker
// thread. If they both fail to do that, we can end up with semi-persistent CPU
// underutilization. The general pattern for goroutine readying is: submit a goroutine
// to local work queue, #StoreLoad-style memory barrier, check sched.nmspinning.
// The general pattern for spinning->non-spinning transition is: decrement nmspinning,
// #StoreLoad-style memory barrier, check all per-P work queues for new work.
// Note that all this complexity does not apply to global run queue as we are not
// sloppy about thread unparking when submitting to global queue. Also see comments
// for nmspinning manipulation.

var (
	// 这两个变量的赋值是汇编实现的. 如: `asm_amd64.s` 的 `TEXT runtime·rt0_go()` 中
	m0           m		// runtime.m0 系统静态分配的 第一个内核
	g0           g		// runtime.g0 系统静态分配的 用来执行引导程序的 g, 被 runtime.m0 执行着  todo (这是 m0中的g0, 但不是 main  goroutine)
	raceprocctx0 uintptr
)

//go:linkname runtime_inittask runtime..inittask
var runtime_inittask initTask

//go:linkname main_inittask main..inittask
var main_inittask initTask

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
var main_init_done chan bool

// main.main()函数入口  main.main()入口
//
//			这个函数只有在 runtime.main() 中被调用  (就是当前文件的 main()中)
//
//go:linkname main_main main.main
func main_main()

// mainStarted indicates that the main M has started.
var mainStarted bool

// runtimeInitTime is the nanotime() at which the runtime started.
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
var initSigmask sigset


// TODO 提供给 G 调用的 main 函数    (参考 runtime.schedinit() )   runtime.main的入口
//
// 		这个 也是  runtime.main() 也就是 我们正常程序中的  main.main()
//
//
//		 main goroutine 在 asm_amd64.s 的 TEXT runtime.rt0_go() 中的, `CALL	runtime·newproc(SB)` 这一行 被创建. 而 该G 的 func() {} 就是现在这个 main()
//
//		 main goroutine 在和 m0 绑定的 P 的本地队列中，那么调用 runqget 的时候 就可以从p的本地队列中获取到 main goroutine ，然后传给 execute 执行，这样 main goroutine就跑起来了.
//
// The main goroutine.
func main() {


	/**
	main 在执行main包中的main函数之前，还是做了一些其他工作包括：
		1. 新建一个线程来执行 sysmon ，sysmon的工作是系统后台监控（定期垃圾回收和调度抢占），下节再详细介绍。
		2. 确保是在主线程上运行
		3. runtime内存init函数的执行，runtime_init 是由编译器动态生成的，里面包含了 runtime 包中所有的 init 函数，感兴趣的同学可以在runtime包中搜 func init() ，会发现有挺多init函数。
		4. 启动gc清扫的goroutine
		5. 执行 main_init 函数，编译器动态生成的，包括用户定义的所有的init函数

	 */

	// 获取 main goroutine
	g := getg()

	// Racectx of m0->g0 is used only as the parent of the main goroutine.
	// It must not be used for anything else.
	g.m.g0.racectx = 0

	// Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
	// Using decimal instead of binary GB and MB because
	// they look nicer in the stack overflow failure message.
	if sys.PtrSize == 8 {
		maxstacksize = 1000000000
	} else {
		maxstacksize = 250000000
	}

	// Allow newproc to start new Ms.
	mainStarted = true

	if GOARCH != "wasm" { // no threads on wasm yet, so no sysmon

		// 在系统栈上运行 sysmon()
		systemstack(func() {

			// 分配一个新的m，运行sysmon系统后台监控
			//  (定期垃圾回收 和 调度抢占)
			newm(sysmon, nil)  // 启动一个新的M执行 `sysmon()` 函数, todo 这个函数会监控全局的状态并对运行时间过长的G进行抢占
		})
	}

	// Lock the main goroutine onto this, the main OS thread,
	// during initialization. Most programs won't care, but a few
	// do require certain calls to be made by the main thread.
	// Those can arrange for main.main to run in the main thread
	// by calling runtime.LockOSThread during initialization
	// to preserve the lock.
	lockOSThread()

	// 确保是 主线程 m0
	if g.m != &m0 {
		throw("runtime.main not on m0")
	}

	// 调用 runtime.init函数   runtime_init函数
	// runtime 内部 init() 函数的执行，编译器动态生成的
	doInit(&runtime_inittask) // must be before defer
	if nanotime() == 0 {
		throw("nanotime returning zero")
	}

	// Defer unlock so that runtime.Goexit during init does the unlock too.
	needUnlock := true
	defer func() {
		if needUnlock {
			unlockOSThread()
		}
	}()

	// Record when the world started.
	runtimeInitTime = nanotime()

	// gc 启动一个goroutine进行gc清扫
	gcenable()  // 启动 后台 GC 清扫工作 (守护进程)

	main_init_done = make(chan bool)
	if iscgo {
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		if GOOS != "windows" {
			if _cgo_setenv == nil {
				throw("_cgo_setenv missing")
			}
			if _cgo_unsetenv == nil {
				throw("_cgo_unsetenv missing")
			}
		}
		if _cgo_notify_runtime_init_done == nil {
			throw("_cgo_notify_runtime_init_done missing")
		}
		// Start the template thread in case we enter Go from
		// a C-created thread and need to create a new thread.
		startTemplateThread()
		cgocall(_cgo_notify_runtime_init_done, nil)
	}

	// todo 执行 init() 函数，编译器动态生成的
	//
	// todo 包括用户定义的所有的init()函数
	doInit(&main_inittask)

	close(main_init_done)

	needUnlock = false
	unlockOSThread()

	if isarchive || islibrary {
		// A program compiled with -buildmode=c-archive or c-shared
		// has a main, but it is not executed.
		return
	}

	// 进行间接调用，因为链接器在放置运行时时不知道主包的地址
	//
	// todo 真正的执行main func in package main
	fn := main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()  // 调用 main.main() 函数
	if raceenabled {
		racefini()
	}

	// 如果当前发生了panic, 则等待panic处理
	//
	// Make racy client program work: if panicking on
	// another goroutine at the same time as main returns,
	// let the other goroutine finish printing the panic trace.
	// Once it does, it will exit. See issues 3934 and 20018.
	if atomic.Load(&runningPanicDefers) != 0 {
		// Running deferred functions should not take long.
		for c := 0; c < 1000; c++ {
			if atomic.Load(&runningPanicDefers) == 0 {
				break
			}
			Gosched()
		}
	}
	if atomic.Load(&panicking) != 0 {
		gopark(nil, nil, waitReasonPanicWait, traceEvGoStop, 1)
	}

	// 调用exit(0)退出程序
	exit(0)

	// 为何这里还需要for循环 ???
	//
	// 下面的for循环一定会导致程序崩掉，这样就确保了程序一定会退出   todo (骚技巧)
	for {
		var x *int32   	// 因为 x 此时 指向的是 nil
		*x = 0			// 所以,  解引用 *nil 肯定导致 崩溃啊
	}
}

// os_beforeExit is called from os.Exit(0).
//go:linkname os_beforeExit os.runtime_beforeExit
func os_beforeExit() {
	if raceenabled {
		racefini()
	}
}

// start forcegc helper goroutine
func init() {
	go forcegchelper()
}

func forcegchelper() {
	forcegc.g = getg()
	for {
		lock(&forcegc.lock)
		if forcegc.idle != 0 {
			throw("forcegc: phase error")
		}
		atomic.Store(&forcegc.idle, 1)
		goparkunlock(&forcegc.lock, waitReasonForceGGIdle, traceEvGoBlock, 1)
		// this goroutine is explicitly resumed by sysmon
		if debug.gctrace > 0 {
			println("GC forced")
		}
		// Time-triggered, fully concurrent.
		gcStart(gcTrigger{kind: gcTriggerTime, now: nanotime()})
	}
}

//go:nosplit

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
func Gosched() {
	checkTimeouts()
	mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//go:nosplit
func goschedguarded() {
	mcall(goschedguarded_m)
}

// Puts the current goroutine into a waiting state and calls unlockf.
// If unlockf returns false, the goroutine is resumed.
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
// Reason explains why the goroutine has been parked.
// It is displayed in stack traces and heap dumps.
// Reasons should be unique and descriptive.
// Do not re-use reasons, add new ones.
//
// todo 将当前 G 进行休眠    (其实是将 M 和 G 解除, 让 M 去运行其他 G)
//
// 将当前goroutine置于【等待状态】，并且调用 unlockf 函数。
// 如果 `unlockf()` 返回false，则继续执行goroutine。
// `unlockf()` 一定不能访问此 G 的堆栈，因为它可能在调用 `gopark()`和调用 `unlockf()` 之间移动。
// 说明了 为什么 goroutine已 parked 的原因。
// 它显示在堆栈跟踪 和 堆转储中。
// 原因应具有唯一性和描述性。
// 不要重复使用原因，请添加新的原因。
//
// 把当前的 goroutine 改为【等待状态】，并且调用 unlockf 函数，如果函数返回 flase，则当前 g 被恢复
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason waitReason, traceEv byte, traceskip int) {
	if reason != waitReasonSleep {
		checkTimeouts() // timeouts may expire while two goroutines keep the scheduler busy
	}
	mp := acquirem()  	// 阻塞当前 G 被抢占   (且返回当前G 的 M)
	gp := mp.curg		// 获取 M 中的 当前G
	status := readgstatus(gp)
	if status != _Grunning && status != _Gscanrunning {
		throw("gopark: bad g status")
	}
	mp.waitlock = lock

	// todo 记住： unlockf 永远返回 true
	mp.waitunlockf = unlockf 	// 外面传入 的 解锁回调函数
	gp.waitreason = reason		// G 被挂起时的 原因
	mp.waittraceev = traceEv
	mp.waittraceskip = traceskip
	releasem(mp)
	// can't do anything that might move the G between Ms here.
	//
	// 无法做任何可能在此处 Ms 之间移动G的操作
	mcall(park_m)   // macll 会先切换成 g0，并把当前 g 作为参数调用  park_m()
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
//
// 将 当前goroutine置于 【等待状态】 并解锁 lock
// 通过调用 goready（gp），可以使goroutine再次可运行
func goparkunlock(lock *mutex, reason waitReason, traceEv byte, traceskip int) {
	gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceEv, traceskip)
}


// todo 将休眠的G 唤醒
func goready(gp *g, traceskip int) {

	// 切换到 g0 调用ready函数, 调用 完切换 回来 当前 g
	systemstack(func() {
		ready(gp, traceskip, true)  // goready() 中被调用
	})
}

// todo 分配 sudog
//
//     发生在 g 被放到 wait队列的 那一刻
//
//go:nosplit
func acquireSudog() *sudog {
	// Delicate dance: the semaphore implementation calls
	// acquireSudog, acquireSudog calls new(sudog),
	// new calls malloc, malloc can call the garbage collector,
	// and the garbage collector calls the semaphore implementation
	// in stopTheWorld.
	// Break the cycle by doing acquirem/releasem around new(sudog).
	// The acquirem/releasem increments m.locks during new(sudog),
	// which keeps the garbage collector from being invoked.
	mp := acquirem()
	pp := mp.p.ptr()
	if len(pp.sudogcache) == 0 {
		lock(&sched.sudoglock)
		// First, try to grab a batch from central cache.
		for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
			s := sched.sudogcache
			sched.sudogcache = s.next
			s.next = nil
			pp.sudogcache = append(pp.sudogcache, s)
		}
		unlock(&sched.sudoglock)
		// If the central cache is empty, allocate a new one.
		if len(pp.sudogcache) == 0 {
			pp.sudogcache = append(pp.sudogcache, new(sudog))
		}
	}
	n := len(pp.sudogcache)
	s := pp.sudogcache[n-1]
	pp.sudogcache[n-1] = nil
	pp.sudogcache = pp.sudogcache[:n-1]
	if s.elem != nil {
		throw("acquireSudog: found s.elem != nil in cache")
	}
	releasem(mp)
	return s
}


// todo 释放  sudog
//
//go:nosplit
func releaseSudog(s *sudog) {
	if s.elem != nil {
		throw("runtime: sudog with non-nil elem")
	}
	if s.isSelect {
		throw("runtime: sudog with non-false isSelect")
	}
	if s.next != nil {
		throw("runtime: sudog with non-nil next")
	}
	if s.prev != nil {
		throw("runtime: sudog with non-nil prev")
	}
	if s.waitlink != nil {
		throw("runtime: sudog with non-nil waitlink")
	}
	if s.c != nil {
		throw("runtime: sudog with non-nil c")
	}
	gp := getg()
	if gp.param != nil {
		throw("runtime: releaseSudog with non-nil gp.param")
	}
	mp := acquirem() // avoid rescheduling to another P
	pp := mp.p.ptr()
	if len(pp.sudogcache) == cap(pp.sudogcache) {
		// Transfer half of local cache to the central cache.
		var first, last *sudog
		for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
			n := len(pp.sudogcache)
			p := pp.sudogcache[n-1]
			pp.sudogcache[n-1] = nil
			pp.sudogcache = pp.sudogcache[:n-1]
			if first == nil {
				first = p
			} else {
				last.next = p
			}
			last = p
		}
		lock(&sched.sudoglock)
		last.next = sched.sudogcache
		sched.sudogcache = first
		unlock(&sched.sudoglock)
	}

	// 将 sodug 壳 丢回 P 的 sudog缓存, 重复利用
	pp.sudogcache = append(pp.sudogcache, s)
	releasem(mp)
}

// funcPC returns the entry PC of the function f.
// It assumes that f is a func value. Otherwise the behavior is undefined.
// CAREFUL: In programs with plugins, funcPC can return different values
// for the same function (because there are actually multiple copies of
// the same function in the address space). To be safe, don't use the
// results of this function in any == expression. It is only safe to
// use the result as an address at which to start executing code.
//
//
// 	todo funcPC() 返回  函数f的入口PC
//	假定f是一个func值.  否则，行为是不确定的.
//
// 	注意：	在带有 `插件<plugin>` 的程序中，funcPC() 可以为同一函数返回不同的值（因为在地址空间中实际上存在同一函数的多个副本）.
// 			为了安全起见，请勿在任何 `==` 表达式中使用此函数的结果. 仅将结果用作开始执行代码的地址是安全的.
//
//
//go:nosplit
func funcPC(f interface{}) uintptr {
	return *(*uintptr)(efaceOf(&f).data)
}

// called from assembly
func badmcall(fn func(*g)) {
	throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
	throw("runtime: mcall function returned")
}

func badreflectcall() {
	panic(plainError("arg size to reflect.call more than 1GB"))
}

var badmorestackg0Msg = "fatal: morestack on g0\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
	sp := stringStructOf(&badmorestackg0Msg)
	write(2, sp.str, int32(sp.len))
}

var badmorestackgsignalMsg = "fatal: morestack on gsignal\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
	sp := stringStructOf(&badmorestackgsignalMsg)
	write(2, sp.str, int32(sp.len))
}

//go:nosplit
func badctxt() {
	throw("ctxt != 0")
}

func lockedOSThread() bool {
	gp := getg()
	return gp.lockedm != 0 && gp.m.lockedg != 0
}

// todo  三个全局的列表主要为了统计运行时系统的的所有G、M、P
var (
	// runtime 的 全局 G 队列 (存放所有 G 指针) todo 新建的G会先放到这个全局的G列表中，其列表的作用也是集中放置了当前运行时系统中给所有的G的指针
	allgs    []*g
	allglock mutex
)


// 将 新G 追加到 全局的 allgs 队列中     (这个方法自由两个 地方在用)
func allgadd(gp *g) {
	if readgstatus(gp) == _Gidle {
		throw("allgadd: bad status Gidle")
	}

	// 锁安全 操作
	lock(&allglock)
	allgs = append(allgs, gp)
	allglen = uintptr(len(allgs))
	unlock(&allglock)
}

const (
	// Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
	// 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
	_GoidCacheBatch = 16
)

// cpuinit extracts the environment variable GODEBUG from the environment on
// Unix-like operating systems and calls internal/cpu.Initialize.
func cpuinit() {
	const prefix = "GODEBUG="
	var env string

	switch GOOS {
	case "aix", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		cpu.DebugOptions = true

		// Similar to goenv_unix but extracts the environment value for
		// GODEBUG directly.
		// TODO(moehrmann): remove when general goenvs() can be called before cpuinit()
		n := int32(0)
		for argv_index(argv, argc+1+n) != nil {
			n++
		}

		for i := int32(0); i < n; i++ {
			p := argv_index(argv, argc+1+i)
			s := *(*string)(unsafe.Pointer(&stringStruct{unsafe.Pointer(p), findnull(p)}))

			if hasPrefix(s, prefix) {
				env = gostring(p)[len(prefix):]
				break
			}
		}
	}

	cpu.Initialize(env)

	// Support cpu feature variables are used in code generated by the compiler
	// to guard execution of instructions that can not be assumed to be always supported.
	x86HasPOPCNT = cpu.X86.HasPOPCNT
	x86HasSSE41 = cpu.X86.HasSSE41
	x86HasFMA = cpu.X86.HasFMA

	armHasVFPv4 = cpu.ARM.HasVFPv4

	arm64HasATOMICS = cpu.ARM64.HasATOMICS
}

// todo 这个才是经过 汇编引导之后，整个go运行起来的核心所在啊   (引导程序)
//
//	 调用 osinit
//	 调用 schedinit
//	 创建一个新的 G并加入 G的全局 queue中
//	 调用 当前G的 mstart()
//
//  一个新的G 会去调用 `runtime.main()` 而当前函数就是创建G的各种初始化
//
//
// The bootstrap sequence is:
//
//	call osinit
//	call schedinit
//	make & queue new G
//	call runtime·mstart
//
// The new G calls runtime·main.----->  调用 runtiem.main() 也就是 当前文件中的 main() 函数
func schedinit() {
	// raceinit must be the first call to race detector.
	// In particular, it must be done before mallocinit below calls racemapshadow.

	// 调用 raceinit 必须是 首次对 race 检测器的调用，特别是：必须在 调用 mallocinit 函数之前，在 racemapshadow() 函数之后调用

	_g_ := getg()
	if raceenabled {
		_g_.racectx, raceprocctx0 = raceinit()
	}

	sched.maxmcount = 10000  // 最大的系统线程数量限制，参考 runtime/debug.SetmaxThreads (这部分在G M P 中有讲)

	tracebackinit()
	moduledataverify()

	// 栈初始化
	stackinit()

	// 内存分配初始化
	mallocinit()
	fastrandinit() // must run before mcommoninit

	// 调度器初始化
	mcommoninit(_g_.m)
	cpuinit()       // must run before alginit
	alginit()       // maps must not be used before this call
	modulesinit()   // provides activeModules
	typelinksinit() // uses maps, activeModules
	itabsinit()     // uses activeModules

	msigsave(_g_.m)
	initSigmask = _g_.m.sigmask

	// 处理命令行参数
	goargs()

	// 处理环境变量
	goenvs()
	parsedebugvars()

	// 垃圾回收器 初始化
	gcinit()

	sched.lastpoll = uint64(nanotime())

	// 使用  CPU的核数 或者 GOMAXPROCS 调试相关的环境变量设置
	procs := ncpu
	if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
		procs = n
	}

	// 调整P数量
	//
	// 调整P的个数，这里是新分配procs个P
	// 这个函数很重要，所有的P都是从这里分配的，以后也不用担心没有P了
	if procresize(procs) != nil {
		throw("unknown runnable goroutine during bootstrap")
	}

	// For cgocheck > 1, we turn on the write barrier at all times
	// and check all pointer writes. We can't do this until after
	// procresize because the write barrier needs a P.
	if debug.cgocheck > 1 {
		writeBarrier.cgo = true
		writeBarrier.enabled = true
		for _, p := range allp {
			p.wbBuf.reset()
		}
	}

	if buildVersion == "" {
		// Condition should never trigger. This code just serves
		// to ensure runtime·buildVersion is kept in the resulting binary.
		buildVersion = "unknown"
	}
	if len(modinfo) == 1 {
		// Condition should never trigger. This code just serves
		// to ensure runtime·modinfo is kept in the resulting binary.
		modinfo = ""
	}
}

func dumpgstatus(gp *g) {
	_g_ := getg()
	print("runtime: gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
	print("runtime:  g:  g=", _g_, ", goid=", _g_.goid, ",  g->atomicstatus=", readgstatus(_g_), "\n")
}

func checkmcount() {
	// sched lock is held
	if mcount() > sched.maxmcount {
		print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
		throw("thread exhaustion")
	}
}

// 初始化 M
func mcommoninit(mp *m) {
	_g_ := getg()

	// g0 stack won't make sense for user (and is not necessary unwindable).
	if _g_ != _g_.m.g0 {
		callers(1, mp.createstack[:])
	}

	lock(&sched.lock)
	if sched.mnext+1 < sched.mnext {
		throw("runtime: thread ID overflow")
	}
	mp.id = sched.mnext
	sched.mnext++
	checkmcount()


	// 给当前M 在求 随机函数时 需要的两个 seed 赋值
	mp.fastrand[0] = uint32(int64Hash(uint64(mp.id), fastrandseed))
	mp.fastrand[1] = uint32(int64Hash(uint64(cputicks()), ^fastrandseed))
	if mp.fastrand[0]|mp.fastrand[1] == 0 {
		mp.fastrand[1] = 1
	}

	mpreinit(mp)
	if mp.gsignal != nil {
		mp.gsignal.stackguard1 = mp.gsignal.stack.lo + _StackGuard
	}

	// Add to allm so garbage collector doesn't free g->m
	// when it is just in a register or thread-local storage.
	mp.alllink = allm

	// NumCgoCall() iterates over allm w/o schedlock,
	// so we need to publish it safely.
	atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
	unlock(&sched.lock)

	// Allocate memory to hold a cgo traceback if the cgo call crashes.
	if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" {
		mp.cgoCallers = new(cgoCallers)
	}
}

var fastrandseed uintptr

func fastrandinit() {
	s := (*[unsafe.Sizeof(fastrandseed)]byte)(unsafe.Pointer(&fastrandseed))[:]
	getRandomData(s)
}

// 唤醒G
//
//
// 将gp的状态更改为_Grunnable，以便调度器调度执行
// 并且如果next==true，那么设置为优先级最高，并尝试wakep
//
//
// Mark gp ready to run.
func ready(gp *g, traceskip int, next bool) {
	if trace.enabled {
		traceGoUnpark(gp, traceskip)
	}

	status := readgstatus(gp)

	// Mark runnable.
	// 此刻的  `_g_` 不是 `gp`
	_g_ := getg()
	mp := acquirem() // disable preemption because it can be holding p in a local var
	if status&^_Gscan != _Gwaiting {
		dumpgstatus(gp)
		throw("bad g->status in ready")
	}

	// status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
	casgstatus(gp, _Gwaiting, _Grunnable)  // 把G的状态由等待中(_Gwaiting)改为待运行(_Grunnable)

	// 把 g 放到 p 本地队列， next 为 true， 就放在下一个执行， next 为 false，放在队尾   todo (刚被唤醒的G 优先级 比在 runq 队列中的G 高)
	runqput(_g_.m.p.ptr(), gp, next)
	// TODO 如果当前 有空闲的P, 但是 无自旋的M(nmspinning等于0), 则 唤醒 或 新建 一个M
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
		wakep() // 唤醒 或 新建 一个M
	}
	releasem(mp)
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
var freezing uint32

// Similar to stopTheWorld but best-effort and can be called several times.
// There is no reverse operation, used during crashing.
// This function must not lock any mutexes.
//
// todo 尝试  冻结 所有动作  类似 stopTheWorld()
//
// 与stopTheWorld相似，但是尽力而为，可以多次调用。
// 没有在崩溃期间使用的反向操作。
// 此函数不得锁定任何互斥锁。
func freezetheworld() {
	atomic.Store(&freezing, 1)
	// stopwait and preemption requests can be lost
	// due to races with concurrently executing threads,
	// so try several times
	for i := 0; i < 5; i++ {
		// this should tell the scheduler to not start any new goroutines
		sched.stopwait = freezeStopWait
		atomic.Store(&sched.gcwaiting, 1)
		// this should stop running goroutines
		if !preemptall() {
			break // no running goroutines
		}
		usleep(1000)
	}
	// to be sure
	usleep(1000)
	preemptall()
	usleep(1000)
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
//go:nosplit
func readgstatus(gp *g) uint32 {
	return atomic.Load(&gp.atomicstatus)
}

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
	success := false

	// Check that transition is valid.
	switch oldval {
	default:
		print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus:top gp->status is not in scan state")
	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscanrunning,
		_Gscansyscall,
		_Gscanpreempted:
		if newval == oldval&^_Gscan {
			success = atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	if !success {
		print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus: gp->status is not in scan state")
	}
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
func castogscanstatus(gp *g, oldval, newval uint32) bool {
	switch oldval {
	case _Grunnable,
		_Grunning,
		_Gwaiting,
		_Gsyscall:
		if newval == oldval|_Gscan {
			return atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
	throw("castogscanstatus")
	panic("not reached")
}

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
	if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
		systemstack(func() {
			print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
			throw("casgstatus: bad incoming values")
		})
	}

	// See https://golang.org/cl/21503 for justification of the yield delay.
	const yieldDelay = 5 * 1000
	var nextYield int64

	// loop if gp->atomicstatus is in a scan state giving
	// GC time to finish and change the state to oldval.
	for i := 0; !atomic.Cas(&gp.atomicstatus, oldval, newval); i++ {
		if oldval == _Gwaiting && gp.atomicstatus == _Grunnable {
			throw("casgstatus: waiting for Gwaiting but is Grunnable")
		}
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			for x := 0; x < 10 && gp.atomicstatus != oldval; x++ {
				procyield(1)
			}
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
//go:nosplit
func casgcopystack(gp *g) uint32 {
	for {
		oldstatus := readgstatus(gp) &^ _Gscan
		if oldstatus != _Gwaiting && oldstatus != _Grunnable {
			throw("copystack: bad status, not Gwaiting or Grunnable")
		}
		if atomic.Cas(&gp.atomicstatus, oldstatus, _Gcopystack) {
			return oldstatus
		}
	}
}

// casGToPreemptScan transitions gp from _Grunning to _Gscan|_Gpreempted.
//
// TODO(austin): This is the only status operation that both changes
// the status and locks the _Gscan bit. Rethink this.
func casGToPreemptScan(gp *g, old, new uint32) {
	if old != _Grunning || new != _Gscan|_Gpreempted {
		throw("bad g transition")
	}
	for !atomic.Cas(&gp.atomicstatus, _Grunning, _Gscan|_Gpreempted) {
	}
}

// casGFromPreempted attempts to transition gp from _Gpreempted to
// _Gwaiting. If successful, the caller is responsible for
// re-scheduling gp.
func casGFromPreempted(gp *g, old, new uint32) bool {
	if old != _Gpreempted || new != _Gwaiting {
		throw("bad g transition")
	}
	return atomic.Cas(&gp.atomicstatus, _Gpreempted, _Gwaiting)
}

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
//
//
// `stopTheWorld（）`停止所有P执行goroutine，在GC安全点中断所有goroutine，并将原因记录为停止原因。
// 					返回时，仅当前goroutine的P正在运行。
// 					不得从系统堆栈中调用`stopTheWorld（）`，并且调用者不得持有 `worldsema`。 当其他 P 恢复执行时，调用者必须调用 `startTheWorld()`
//
// `stopTheWorld（）` 对于多个goroutine同时调用是安全的。 每个将执行自己的停止，并且停止将被序列化。
//
// 进行堆栈转储的例程也使用此方法。 如果系统处于紧急状态或退出系统，则可能无法可靠地停止所有goroutine。
func stopTheWorld(reason string) {

	// 挂起 监听 worldsema 信号量 的协程
	semacquire(&worldsema)

	// 将 stop 的 原因写入当前 g 的 m
	getg().m.preemptoff = reason

	// 调用 核心代码 `stopTheWorldWithSema()`
	systemstack(stopTheWorldWithSema)
}

// startTheWorld undoes the effects of stopTheWorld.
//
// startTheWorld() 撤消 stopTheWorld() 的影响
func startTheWorld() {

	// 调用 核心代码 `startTheWorldWithSema()`
	systemstack(func() { startTheWorldWithSema(false) })
	// worldsema must be held over startTheWorldWithSema to ensure
	// gomaxprocs cannot change while worldsema is held.
	//
	// 必须在 startTheWorldWithSema() 上保留 worldsema， 以确保在握持worldsema时gomaxprocs不会改变
	//
	// 释放 监听 worldsema 信号量 的协程
	semrelease(&worldsema)

	// 将 当前 G 被挂起的原因 清空
	getg().m.preemptoff = ""
}

// Holding worldsema grants an M the right to try to stop the world
// and prevents gomaxprocs from changing concurrently.
var worldsema uint32 = 1

// stopTheWorldWithSema is the core implementation of stopTheWorld.
// The caller is responsible for acquiring worldsema and disabling
// preemption first and then should stopTheWorldWithSema on the system
// stack:
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "reason"
//	systemstack(stopTheWorldWithSema)
//
// When finished, the caller must either call startTheWorld or undo
// these three operations separately:
//
//	m.preemptoff = ""
//	systemstack(startTheWorldWithSema)
//	semrelease(&worldsema)
//
// It is allowed to acquire worldsema once and then execute multiple
// startTheWorldWithSema/stopTheWorldWithSema pairs.
// Other P's are able to execute between successive calls to
// startTheWorldWithSema and stopTheWorldWithSema.
// Holding worldsema causes any other goroutines invoking
// stopTheWorld to block.
//
//
// todo 真正的 stopTheWorld 的核心实现逻辑
//
//
// 	stopTheWorldWithSema() 是 stopTheWorld（）的核心实现。
//
// 	调用方负责获取 worldsema 并先禁用抢占，然后应在系统上停止 TheWorldWithSema
// 	堆栈：
//
// 			semacquire（＆worldsema，0）
// 			m.preemptoff =“原因”
// 			systemstack（stopTheWorldWithSema）
//
//	完成后，调用方必须调用 `startTheWorld()` 或 分别撤消以下三个操作：
//
// 		m.preemptoff = ""
// 		systemstack(startTheWorldWithSema)
// 		semrelease(&worldsema)
//
//	一次获取worldsema，然后执行多个 `startTheWorldWithSema() / stopTheWorldWithSema()` 对。
//	其他P 可以在连续调用 startTheWorldWithSema 和 stopTheWorldWithSema 之间执行。
//	持有 worldsema 会导致其他任何调用 stopTheWorld() 的goroutine阻塞。
func stopTheWorldWithSema() {

	// 获取 当前 G
	_g_ := getg()

	// If we hold a lock, then we won't be able to stop another M
	// that is blocked trying to acquire the lock.
	//
	// 如果我们持有锁，那么我们将无法停止试图获取锁 而被阻止的另一个M
	if _g_.m.locks > 0 {
		throw("stopTheWorld: holding locks")  // 还有 M 在运作
	}

	lock(&sched.lock)

	// 需要停止的P数量
	sched.stopwait = gomaxprocs   // 直接将 最大 P 总数 记录起来

	// 设置gc等待标记, 调度时看见此标记会进入等待
	atomic.Store(&sched.gcwaiting, 1)

	// 抢占所有运行中的G
	preemptall()

	// 停止当前的P
	// stop current P
	_g_.m.p.ptr().status = _Pgcstop // Pgcstop is only diagnostic.

	// 减少需要停止的P数量(当前的P算一个)
	sched.stopwait--

	// 抢占所有在Psyscall状态的P, 防止它们重新参与调度
	//
	// try to retake all P's in Psyscall status
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && atomic.Cas(&p.status, s, _Pgcstop) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			sched.stopwait--
		}
	}

	// 防止所有空闲的P重新参与调度
	//
	// stop idle P's
	for {
		p := pidleget()
		if p == nil {
			break
		}
		p.status = _Pgcstop
		sched.stopwait--
	}
	wait := sched.stopwait > 0
	unlock(&sched.lock)

	// 如果仍有需要停止的P, 则等待它们停止
	//
	// wait for remaining P's to stop voluntarily
	if wait {
		for {

			// 循环等待 + 抢占所有运行中的G
			//
			// wait for 100us, then try to re-preempt in case of any races
			if notetsleep(&sched.stopnote, 100*1000) {
				noteclear(&sched.stopnote)
				break
			}
			preemptall()
		}
	}

	// 逻辑正确性检查
	//
	// sanity checks
	bad := ""
	if sched.stopwait != 0 {
		bad = "stopTheWorld: not stopped (stopwait != 0)"
	} else {
		for _, p := range allp {
			if p.status != _Pgcstop {
				bad = "stopTheWorld: not stopped (status != _Pgcstop)"
			}
		}
	}
	if atomic.Load(&freezing) != 0 {
		// Some other thread is panicking. This can cause the
		// sanity checks above to fail if the panic happens in
		// the signal handler on a stopped thread. Either way,
		// we should halt this thread.
		lock(&deadlock)
		lock(&deadlock)
	}
	if bad != "" {
		throw(bad)
	}

	// 到这里所有运行中的G都会变为 【待运行】, 并且所有的P都不能被M获取
	// 也就是说所有的go代码(除了当前的)都会停止运行, 并且不能运行新的go代码
}


// todo 启动 GC 之后的 停止世界
func startTheWorldWithSema(emitTraceEvent bool) int64 {
	mp := acquirem() // disable preemption because it can be holding p in a local var
	if netpollinited() {
		list := netpoll(0) // non-blocking
		injectglist(&list)
	}
	lock(&sched.lock)


	// 如果要求改变gomaxprocs则调整P的数量
	// procresize会返回有可运行任务的P的链表
	procs := gomaxprocs
	if newprocs != 0 {
		procs = newprocs
		newprocs = 0
	}
	p1 := procresize(procs)

	// 取消GC等待标记
	sched.gcwaiting = 0

	// 如果 sysmon 在等待则唤醒它
	if sched.sysmonwait != 0 {
		sched.sysmonwait = 0
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)


	// 唤醒 有可运行任务的P
	for p1 != nil {
		p := p1
		p1 = p1.link.ptr()
		if p.m != 0 {
			mp := p.m.ptr()
			p.m = 0
			if mp.nextp != 0 {
				throw("startTheWorld: inconsistent mp->nextp")
			}
			mp.nextp.set(p)
			notewakeup(&mp.park)
		} else {
			// Start M to run P.  Do not start another M below.
			newm(nil, p)
		}
	}

	// Capture start-the-world time before doing clean-up tasks.
	startTime := nanotime()
	if emitTraceEvent {
		traceGCSTWDone()
	}

	// 如果有空闲的P，并且没有自旋中的M  则 唤醒 或者 创建 一个M
	//
	//
	// Wakeup an additional proc in case we have excessive runnable goroutines
	// in local queues or in the global queue. If we don't, the proc will park itself.
	// If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
		wakep()
	}

	releasem(mp)

	return startTime
}

// mstart is the entry-point for new Ms.
//
// This must not split the stack because we may not even have stack
// bounds set up yet.
//
// May run during STW (because it doesn't have a P yet), so write
// barriers are not allowed.
//
// todo M 的调度
//
// `mstart()` 是 新 Ms 的入口点
//
//     M启动时会调用mstart函数, m0 在初始化后调用, 其他的的 m 在线程启动后调用.
//
//   这不能拆分堆栈，因为我们甚至可能 尚未 设置堆栈边界
//
//   可能在STW期间运行 (因为它还没有P) 因此不允许写障碍
//
//
//go:nosplit
//go:nowritebarrierrec
func mstart() {

	// 这里 获取的是 g0
	_g_ := getg()

	// 如果 g 未分配栈则从当前的栈空间(系统栈空间)上分配, 也就是说g0会使用系统栈空间
	osStack := _g_.stack.lo == 0
	if osStack {
		// Initialize stack bounds from system stack.
		// Cgo may have left stack size in stack.hi.
		// minit may update the stack bounds.
		size := _g_.stack.hi
		if size == 0 {
			size = 8192 * sys.StackGuardMultiplier
		}
		_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
		_g_.stack.lo = _g_.stack.hi - size + 1024
	}
	// Initialize stack guard so that we can start calling regular
	// Go code.
	_g_.stackguard0 = _g_.stack.lo + _StackGuard
	// This is the g0, so we can also call go:systemstack
	// functions, which check stackguard1.
	_g_.stackguard1 = _g_.stackguard0
	mstart1()  // todo 这个方法会被 阻塞

	// Exit this thread.
	switch GOOS {
	case "windows", "solaris", "illumos", "plan9", "darwin", "aix":
		// Windows, Solaris, illumos, Darwin, AIX and Plan 9 always system-allocate
		// the stack, but put it in _g_.stack before mstart,
		// so the logic above hasn't set osStack yet.
		osStack = true
	}
	mexit(osStack)
}

func mstart1() {
	_g_ := getg()

	// 这里还是 g0
	if _g_ != _g_.m.g0 {
		throw("bad runtime·mstart")
	}

	// 调用 gosave 函数 保存当前的状态 到g0的调度数据中, 以后每次调度都会从这个栈地址开始.
	//
	//
	// Record the caller for use as the top of stack in mcall and
	// for terminating the thread.
	// We're never coming back to mstart1 after we call schedule,
	// so other calls can reuse the current frame.
	save(getcallerpc(), getcallersp())
	asminit()		// 调用asminit函数, 不做任何事情
	minit()			// 调用minit函数, 设置当前线程可以接收的信号(signal)

	// Install signal handlers; after minit so that minit can
	// prepare the thread to be able to handle the signals.
	//
	// 安装信号处理程序; `minit()` 之后，以便 `minit()` 可以准备线程以处理信号
	if _g_.m == &m0 {
		mstartm0()  // 运行全局的 m0
	}

	if fn := _g_.m.mstartfn; fn != nil {
		fn()
	}

	if _g_.m != &m0 {
		acquirep(_g_.m.nextp.ptr())
		_g_.m.nextp = 0
	}
	schedule()   // todo 在 mstart() 中, 发起 【一轮调度程序】：找到一个可运行的goroutine并执行它
	// schedule函数获取g => [必要时休眠] => [唤醒后继续获取] => execute函数执行g => 执行后返回到goexit => 重新执行schedule函数
}

// mstartm0 implements part of mstart1 that only runs on the m0.
//
// Write barriers are allowed here because we know the GC can't be
// running yet, so they'll be no-ops.
//
// todo 运行全局的 m0
//
// `mstartm0()` 实现 `mstart1()` 的一部分，该部分仅在 m0 上运行
//
//  这里允许写屏障，因为我们知道GC尚未运行，因此它们将成为无操作
//
//
//go:yeswritebarrierrec
func mstartm0() {
	// Create an extra M for callbacks on threads not created by Go.
	// An extra M is also needed on Windows for callbacks created by
	// syscall.NewCallback. See issue #6751 for details.
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		cgoHasExtraM = true
		newextram()
	}
	initsig(false)
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&_g_.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
//
//go:yeswritebarrierrec
func mexit(osStack bool) { // m 退出
	g := getg()
	m := g.m

	if m == &m0 {
		// This is the main thread. Just wedge it.
		//
		// On Linux, exiting the main thread puts the process
		// into a non-waitable zombie state. On Plan 9,
		// exiting the main thread unblocks wait even though
		// other threads are still running. On Solaris we can
		// neither exitThread nor return from mstart. Other
		// bad things probably happen on other platforms.
		//
		// We could try to clean up this M more before wedging
		// it, but that complicates signal handling.
		handoffp(releasep())
		lock(&sched.lock)
		sched.nmfreed++
		checkdead()
		unlock(&sched.lock)
		notesleep(&m.park)
		throw("locked m0 woke up")
	}

	sigblock()
	unminit()

	// Free the gsignal stack.
	if m.gsignal != nil {
		stackfree(m.gsignal.stack)
		// On some platforms, when calling into VDSO (e.g. nanotime)
		// we store our g on the gsignal stack, if there is one.
		// Now the stack is freed, unlink it from the m, so we
		// won't write to it when calling VDSO code.
		m.gsignal = nil
	}

	// Remove m from allm.
	lock(&sched.lock)
	for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
		if *pprev == m {
			*pprev = m.alllink
			goto found
		}
	}
	throw("m not found in allm")
found:
	if !osStack {
		// Delay reaping m until it's done with the stack.
		//
		// If this is using an OS stack, the OS will free it
		// so there's no need for reaping.
		atomic.Store(&m.freeWait, 1)
		// Put m on the free list, though it will not be reaped until
		// freeWait is 0. Note that the free list must not be linked
		// through alllink because some functions walk allm without
		// locking, so may be using alllink.
		//
		// 将 m 放到 sched 的 freem链表上. (这些都是调用 mexit() 的 m)
		m.freelink = sched.freem
		sched.freem = m
	}
	unlock(&sched.lock)

	// Release the P.
	handoffp(releasep())
	// After this point we must not have write barriers.

	// Invoke the deadlock detector. This must happen after
	// handoffp because it may have started a new M to take our
	// P's work.
	lock(&sched.lock)
	sched.nmfreed++
	checkdead()
	unlock(&sched.lock)

	if osStack {
		// Return from mstart and let the system thread
		// library free the g0 stack and terminate the thread.
		return
	}

	// mstart is the thread's entry point, so there's nothing to
	// return to. Exit the thread directly. exitThread will clear
	// m.freeWait when it's done with the stack and the m can be
	// reaped.
	exitThread(&m.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema.
//
//go:systemstack
func forEachP(fn func(*p)) {
	mp := acquirem()
	_p_ := getg().m.p.ptr()

	lock(&sched.lock)
	if sched.safePointWait != 0 {
		throw("forEachP: sched.safePointWait != 0")
	}
	sched.safePointWait = gomaxprocs - 1
	sched.safePointFn = fn

	// Ask all Ps to run the safe point function.
	for _, p := range allp {
		if p != _p_ {
			atomic.Store(&p.runSafePointFn, 1)
		}
	}
	preemptall()

	// Any P entering _Pidle or _Psyscall from now on will observe
	// p.runSafePointFn == 1 and will call runSafePointFn when
	// changing its status to _Pidle/_Psyscall.

	// Run safe point function for all idle Ps. sched.pidle will
	// not change because we hold sched.lock.
	for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
		if atomic.Cas(&p.runSafePointFn, 1, 0) {
			fn(p)
			sched.safePointWait--
		}
	}

	wait := sched.safePointWait > 0
	unlock(&sched.lock)

	// Run fn for the current P.
	fn(_p_)

	// Force Ps currently in _Psyscall into _Pidle and hand them
	// off to induce safe point function execution.
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && p.runSafePointFn == 1 && atomic.Cas(&p.status, s, _Pidle) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			handoffp(p)
		}
	}

	// Wait for remaining Ps to run fn.
	if wait {
		for {
			// Wait for 100us, then try to re-preempt in
			// case of any races.
			//
			// Requires system stack.
			if notetsleep(&sched.safePointNote, 100*1000) {
				noteclear(&sched.safePointNote)
				break
			}
			preemptall()
		}
	}
	if sched.safePointWait != 0 {
		throw("forEachP: not done")
	}
	for _, p := range allp {
		if p.runSafePointFn != 0 {
			throw("forEachP: P did not run fn")
		}
	}

	lock(&sched.lock)
	sched.safePointFn = nil
	unlock(&sched.lock)
	releasem(mp)
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//
//     if getg().m.p.runSafePointFn != 0 {
//         runSafePointFn()
//     }
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
func runSafePointFn() {
	p := getg().m.p.ptr()
	// Resolve the race between forEachP running the safe-point
	// function on this P's behalf and this P running the
	// safe-point function directly.
	if !atomic.Cas(&p.runSafePointFn, 1, 0) {
		return
	}
	sched.safePointFn(p)
	lock(&sched.lock)
	sched.safePointWait--
	if sched.safePointWait == 0 {
		notewakeup(&sched.safePointNote)
	}
	unlock(&sched.lock)
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
	g   guintptr
	tls *uint64
	fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows _p_.
//
//	分配与任何线程无关的新m。
//	如果需要，可以将p用于分配上下文
// 	fn被记录为新m的m.mstartfn
//
//	此函数即使没有调用者也被允许有写障碍，因为它借用 _p_
//
//go:yeswritebarrierrec
func allocm(_p_ *p, fn func()) *m {

	// 获取 当前G
	_g_ := getg()
	acquirem() // disable GC because it can be called from sysmon   禁用GC，因为可以从sysmon调用它

	// 如果 g 的M 没有绑定 P
	if _g_.m.p == 0 {
		acquirep(_p_) // temporarily borrow p for mallocs in this function   在此函数中临时为mallocs借用p
	}

	// Release the free M list. We need to do this somewhere and
	// this may free up a stack we can use.
	//
	// 释放 schedt的 【空闲】M列表。 我们需要在某个地方执行此操作，这可能会释放我们可以使用的堆栈
	if sched.freem != nil {
		lock(&sched.lock)
		var newList *m

		// 逐个遍历 schedt 的 【空闲】M 列表
		for freem := sched.freem; freem != nil; {
			if freem.freeWait != 0 {
				next := freem.freelink
				freem.freelink = newList
				newList = freem
				freem = next
				continue
			}

			// 释放 M 中的 堆栈
			stackfree(freem.g0.stack)
			freem = freem.freelink
		}
		sched.freem = newList
		unlock(&sched.lock)
	}


	// todo  创建一个 新M 指针
	mp := new(m)
	mp.mstartfn = fn  	// 赋值 mstartfn 函数
	mcommoninit(mp)		// 初始化 M

	// In case of cgo or Solaris or illumos or Darwin, pthread_create will make us a stack.
	// Windows and Plan 9 will layout sched stack on OS stack.
	if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" || GOOS == "plan9" || GOOS == "darwin" {
		mp.g0 = malg(-1)
	} else {
		mp.g0 = malg(8192 * sys.StackGuardMultiplier)
	}
	mp.g0.m = mp

	if _p_ == _g_.m.p.ptr() {
		releasep()
	}
	releasem(_g_.m)

	return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via Casuintptr) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
//
// When the callback is done with the m, it calls dropm to
// put the m back on the list.
//go:nosplit
func needm(x byte) {
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		// Can happen if C/C++ code calls Go from a global ctor.
		// Can also happen on Windows if a global ctor uses a
		// callback created by syscall.NewCallback. See issue #6751
		// for details.
		//
		// Can not throw, because scheduler is not initialized yet.
		write(2, unsafe.Pointer(&earlycgocallback[0]), int32(len(earlycgocallback)))
		exit(1)
	}

	// Lock extra list, take head, unlock popped list.
	// nilokay=false is safe here because of the invariant above,
	// that the extra list always contains or will soon contain
	// at least one m.
	mp := lockextra(false)

	// Set needextram when we've just emptied the list,
	// so that the eventual call into cgocallbackg will
	// allocate a new m for the extra list. We delay the
	// allocation until then so that it can be done
	// after exitsyscall makes sure it is okay to be
	// running at all (that is, there's no garbage collection
	// running right now).
	mp.needextram = mp.schedlink == 0
	extraMCount--
	unlockextra(mp.schedlink.ptr())

	// Save and block signals before installing g.
	// Once g is installed, any incoming signals will try to execute,
	// but we won't have the sigaltstack settings and other data
	// set up appropriately until the end of minit, which will
	// unblock the signals. This is the same dance as when
	// starting a new m to run Go code via newosproc.
	msigsave(mp)
	sigblock()

	// Install g (= m->g0) and set the stack bounds
	// to match the current stack. We don't actually know
	// how big the stack is, like we don't know how big any
	// scheduling stack is, but we assume there's at least 32 kB,
	// which is more than enough for us.
	setg(mp.g0)
	_g_ := getg()
	_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&x))) + 1024
	_g_.stack.lo = uintptr(noescape(unsafe.Pointer(&x))) - 32*1024
	_g_.stackguard0 = _g_.stack.lo + _StackGuard

	// Initialize this thread to use the m.
	asminit()
	minit()

	// mp.curg is now a real goroutine.
	casgstatus(mp.curg, _Gdead, _Gsyscall)
	atomic.Xadd(&sched.ngsys, -1)
}

var earlycgocallback = []byte("fatal error: cgo callback before cgo call\n")

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
func newextram() {
	c := atomic.Xchg(&extraMWaiters, 0)
	if c > 0 {
		for i := uint32(0); i < c; i++ {
			oneNewExtraM()
		}
	} else {
		// Make sure there is at least one extra M.
		mp := lockextra(true)
		unlockextra(mp)
		if mp == nil {
			oneNewExtraM()
		}
	}
}

// oneNewExtraM allocates an m and puts it on the extra list.
func oneNewExtraM() {
	// Create extra goroutine locked to extra m.
	// The goroutine is the context in which the cgo callback will run.
	// The sched.pc will never be returned to, but setting it to
	// goexit makes clear to the traceback routines where
	// the goroutine stack ends.
	mp := allocm(nil, nil)
	gp := malg(4096)
	gp.sched.pc = funcPC(goexit) + sys.PCQuantum
	gp.sched.sp = gp.stack.hi
	gp.sched.sp -= 4 * sys.RegSize // extra space in case of reads slightly beyond frame
	gp.sched.lr = 0
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.syscallpc = gp.sched.pc
	gp.syscallsp = gp.sched.sp
	gp.stktopsp = gp.sched.sp
	// malg returns status as _Gidle. Change to _Gdead before
	// adding to allg where GC can see it. We use _Gdead to hide
	// this from tracebacks and stack scans since it isn't a
	// "real" goroutine until needm grabs it.
	casgstatus(gp, _Gidle, _Gdead)
	gp.m = mp
	mp.curg = gp
	mp.lockedInt++
	mp.lockedg.set(gp)
	gp.lockedm.set(mp)
	gp.goid = int64(atomic.Xadd64(&sched.goidgen, 1))
	if raceenabled {
		gp.racectx = racegostart(funcPC(newextram) + sys.PCQuantum)
	}
	// put on allg for garbage collector
	allgadd(gp)

	// gp is now on the allg list, but we don't want it to be
	// counted by gcount. It would be more "proper" to increment
	// sched.ngfree, but that requires locking. Incrementing ngsys
	// has the same effect.
	atomic.Xadd(&sched.ngsys, +1)

	// Add m to the extra list.
	mnext := lockextra(true)
	mp.schedlink.set(mnext)
	extraMCount++
	unlockextra(mp)
}

// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
// It puts the current m back onto the extra list.
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
//
// TODO(rsc): An alternative would be to allocate a dummy pthread per-thread
// variable using pthread_key_create. Unlike the pthread keys we already use
// on OS X, this dummy key would never be read by Go code. It would exist
// only so that we could register at thread-exit-time destructor.
// That destructor would put the m back onto the extra list.
// This is purely a performance optimization. The current version,
// in which dropm happens on each cgo call, is still correct too.
// We may have to keep the current version on systems with cgo
// but without pthreads, like Windows.
func dropm() {
	// Clear m and g, and return m to the extra list.
	// After the call to setg we can only call nosplit functions
	// with no pointer manipulation.
	mp := getg().m

	// Return mp.curg to dead state.
	casgstatus(mp.curg, _Gsyscall, _Gdead)
	mp.curg.preemptStop = false
	atomic.Xadd(&sched.ngsys, +1)

	// Block signals before unminit.
	// Unminit unregisters the signal handling stack (but needs g on some systems).
	// Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
	// It's important not to try to handle a signal between those two steps.
	sigmask := mp.sigmask
	sigblock()
	unminit()

	mnext := lockextra(true)
	extraMCount++
	mp.schedlink.set(mnext)

	setg(nil)

	// Commit the release of mp.
	unlockextra(mp)

	msigrestore(sigmask)
}

// A helper function for EnsureDropM.
func getm() uintptr {
	return uintptr(unsafe.Pointer(getg().m))
}

var extram uintptr
var extraMCount uint32 // Protected by lockextra
var extraMWaiters uint32

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//go:nosplit
func lockextra(nilokay bool) *m {
	const locked = 1

	incr := false
	for {
		old := atomic.Loaduintptr(&extram)
		if old == locked {
			yield := osyield
			yield()
			continue
		}
		if old == 0 && !nilokay {
			if !incr {
				// Add 1 to the number of threads
				// waiting for an M.
				// This is cleared by newextram.
				atomic.Xadd(&extraMWaiters, 1)
				incr = true
			}
			usleep(1)
			continue
		}
		if atomic.Casuintptr(&extram, old, locked) {
			return (*m)(unsafe.Pointer(old))
		}
		yield := osyield
		yield()
		continue
	}
}

//go:nosplit
func unlockextra(mp *m) {
	atomic.Storeuintptr(&extram, uintptr(unsafe.Pointer(mp)))
}

// execLock serializes exec and clone to avoid bugs or unspecified behaviour
// around exec'ing while creating/destroying threads.  See issue #19546.
var execLock rwmutex

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
var newmHandoff struct {
	lock mutex

	// newm points to a list of M structures that need new OS
	// threads. The list is linked through m.schedlink.
	newm muintptr

	// waiting indicates that wake needs to be notified when an m
	// is put on the list.
	waiting bool
	wake    note

	// haveTemplateThread indicates that the templateThread has
	// been started. This is not protected by lock. Use cas to set
	// to 1.
	haveTemplateThread uint32
}

// Create a new m. It will start off with a call to fn, or else the scheduler.
// fn needs to be static and not a heap allocated closure.
// May run with m.p==nil, so write barriers are not allowed.
//
//	todo 创建一个新的M     新建M
//
// 		它将以调用`fn`或 调度程序 开始
//		`fn`必须是静态的，而不是堆分配的闭包
//
//		可以与m.p == nil一起运行，因此不允许 写屏障
//
//  新建M是通过 `newm()` 来创建的，最终是通过 `newosproc()` 函数来实现的新建线程，不通平台的 `newosproc()` 有不同的具体实现,
// 			比如linux平台下，是调用 【clone 系统调用来实现创建线程】.
// 			新建线程的任务函数为 mstart()，所以当线程启动的时候，是执行 mstart() 函数的代码.
//
//go:nowritebarrierrec
func newm(fn func(), _p_ *p) {
	// 根据 fn 和 p 和 绑定一个m对象
	mp := allocm(_p_, fn)
	// 设置当前m的下一个p为_p_
	mp.nextp.set(_p_)
	mp.sigmask = initSigmask
	if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
		// We're on a locked M or a thread that may have been
		// started by C. The kernel state of this thread may
		// be strange (the user may have locked it for that
		// purpose). We don't want to clone that into another
		// thread. Instead, ask a known-good thread to create
		// the thread for us.
		//
		// This is disabled on Plan 9. See golang.org/issue/22227.
		//
		// TODO: This may be unnecessary on Windows, which
		// doesn't model thread creation off fork.
		//
		//
		// 我们处于锁定的M 或 可能由C启动的线程.  此线程的内核状态可能很奇怪 (用户可能已将其锁定为此目的).
		//
		// 我们不想将其克隆到另一个线程中. 相反, todo 请求一个已知良好的线程为我们创建线程.
		lock(&newmHandoff.lock)
		if newmHandoff.haveTemplateThread == 0 {
			throw("on a locked thread with no template thread")
		}
		mp.schedlink = newmHandoff.newm
		newmHandoff.newm.set(mp)
		if newmHandoff.waiting {
			newmHandoff.waiting = false
			notewakeup(&newmHandoff.wake)
		}
		unlock(&newmHandoff.lock)
		return
	}
	newm1(mp)  // 真正的分配os thread (给M分配内核线程)
}
// 真正的分配os thread
func newm1(mp *m) { // (给M分配内核线程)

	// 对cgo的处理
	if iscgo {
		var ts cgothreadstart
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		ts.g.set(mp.g0)
		ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0]))
		ts.fn = unsafe.Pointer(funcPC(mstart))
		if msanenabled {
			msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}
		execLock.rlock() // Prevent process clone.
		asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts))
		execLock.runlock()
		return
	}
	execLock.rlock() // Prevent process clone.

	// 创建一个系统线程 (内核线程) ，并且传入该 mp 绑定的 g0 的栈顶指针
	// 让系统线程执行 mstart() 函数，后面的逻辑都在 mstart() 函数中
	newosproc(mp)
	execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
func startTemplateThread() {
	if GOARCH == "wasm" { // no threads on wasm yet
		return
	}

	// Disable preemption to guarantee that the template thread will be
	// created before a park once haveTemplateThread is set.
	mp := acquirem()
	if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
		releasem(mp)
		return
	}
	newm(templateThread, nil)
	releasem(mp)
}

// templateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be in a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
//go:nowritebarrierrec
func templateThread() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	for {
		lock(&newmHandoff.lock)
		for newmHandoff.newm != 0 {
			newm := newmHandoff.newm.ptr()
			newmHandoff.newm = 0
			unlock(&newmHandoff.lock)
			for newm != nil {
				next := newm.schedlink.ptr()
				newm.schedlink = 0
				newm1(newm)
				newm = next
			}
			lock(&newmHandoff.lock)
		}
		newmHandoff.waiting = true
		noteclear(&newmHandoff.wake)
		unlock(&newmHandoff.lock)
		notesleep(&newmHandoff.wake)
	}
}

//
// 停止执行当前的 M，直到有新工作可用
// 返回 M 中的 P
//
// Stops execution of the current m until new work is available.
// Returns with acquired P.
func stopm() {

	// 获取当前G
	_g_ := getg()

	// 这时候 M 中的 G 的抢占锁 应该都 被释放了才对
	if _g_.m.locks != 0 {
		throw("stopm holding locks")
	}

	// M 中的 P 应该被释放了才对
	if _g_.m.p != 0 {
		throw("stopm holding p")
	}
	if _g_.m.spinning {
		throw("stopm spinning")
	}

	lock(&sched.lock)
	mput(_g_.m)   // 将 当前 空闲M  放到 全局schedt 的空闲 M 队列中
	unlock(&sched.lock)
	// 等到另一个线程再次调度 lockedg   (说白了, 就是等待当前 M 被别的 G 释放掉出来)     （看  stoplockdm() 函数中 这两部的做法）
	notesleep(&_g_.m.park)     	// 通知 M 休眠   (使用 信号灯)
	noteclear(&_g_.m.park)		// todo 清除 信号灯的标识位,  唤醒监听者 ??
	acquirep(_g_.m.nextp.ptr())		// M 被唤醒后,  尝试性 去绑定 潜在的 P
	_g_.m.nextp = 0					// nextp  转移到  p 字段了,  则清除掉 nextp
}

func mspinning() {
	// startm's caller incremented nmspinning. Set the new M's spinning.
	//
	// `startm()` 的 调用者 增加了nmspinning. 设置新的M 自旋
	getg().m.spinning = true
}

// startm是启动一个M，先尝试获取一个空闲P，如果获取不到则返回
//
// 获取到P后，在尝试获取M，如果获取不到就新建一个M
//
//
// Schedules some M to run the p (creates an M if necessary).
// If p==nil, tries to get an idle P, if no idle P's does nothing.
// May run with m.p==nil, so write barriers are not allowed.
// If spinning is set, the caller has incremented nmspinning and startm will
// either decrement nmspinning or set m.spinning in the newly started M.
//go:nowritebarrierrec
func startm(_p_ *p, spinning bool) {

	// 锁住 当前 schedt
	lock(&sched.lock)

	// 如果 P 为空, 则 从  schedt 的 pidle 队列获取一个 【空闲】P
	if _p_ == nil {
		_p_ = pidleget()

		// 如果 没有 【空闲】的 P
		if _p_ == nil {
			unlock(&sched.lock)

			if spinning {
				// The caller incremented nmspinning, but there are no idle Ps,
				// so it's okay to just undo the increment and give up.
				if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
					throw("startm: negative nmspinning")
				}
			}
			return
		}
	}


	// 获取 【空闲】 M
	mp := mget()
	unlock(&sched.lock)

	// 如果 没有 【空闲】 M
	if mp == nil {
		var fn func()
		if spinning {
			// The caller incremented nmspinning, so set m.spinning in the new M.
			fn = mspinning
		}

		// 如果获取不到，则新建一个，新建完成后就立即返回
		newm(fn, _p_)
		return
	}

	// 到这里表示获取到了一个空闲M

	// 从midle中获取的mp，不应该是spinning状态，获取的都是经过stopm的，stopm之前都会推出spinning
	if mp.spinning {
		throw("startm: m is spinning")
	}
	// 这个位置是要留给参数 _p_ 的，stopm中如果被唤醒，则关联nextp和m
	if mp.nextp != 0 {
		throw("startm: m has p")
	}
	// spinning状态的M是在本地和全局都获取不到工作的情况，不能与spinning语义矛盾
	if spinning && !runqempty(_p_) {
		throw("startm: p has runnable gs")
	}
	// The caller incremented nmspinning, so set m.spinning in the new M.
	mp.spinning = spinning			// 标记该M是否在自旋
	mp.nextp.set(_p_)				// 设置 M 被唤醒时， 暂存P 为 可潜在关联的 P
	notewakeup(&mp.park)			// 唤醒 M 对应的 内核线程
}

// Hands off P from syscall or locked M.
// Always runs without a P, so write barriers are not allowed.
//
//  todo  让出P, 让出 M 上的 P
//
// todo 解除M和P之间的关联
//
// 将P 从系统调用中 移开 或 锁定M
//
// 当本线程 M 因为 G进行【系统调用】而阻塞时，线程释放绑定的P，把P转移给其他空闲的线程执行
//
// 始终在不带P 的情况下运行，因此不允许 写入屏障
//
//
//go:nowritebarrierrec
func handoffp(_p_ *p) {
	// handoffp must start an M in any situation where
	// findrunnable would return a G to run on _p_.

	// if it has local work, start it straight away
	if !runqempty(_p_) || sched.runqsize != 0 {
		startm(_p_, false)
		return
	}
	// if it has GC work, start it straight away
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(_p_) {
		startm(_p_, false)
		return
	}
	// no local work, check that there are no spinning/idle M's,
	// otherwise our help is not required
	if atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) == 0 && atomic.Cas(&sched.nmspinning, 0, 1) { // TODO: fast atomic
		startm(_p_, true)
		return
	}
	lock(&sched.lock)
	if sched.gcwaiting != 0 {
		_p_.status = _Pgcstop
		sched.stopwait--
		if sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
		unlock(&sched.lock)
		return
	}
	if _p_.runSafePointFn != 0 && atomic.Cas(&_p_.runSafePointFn, 1, 0) {
		sched.safePointFn(_p_)
		sched.safePointWait--
		if sched.safePointWait == 0 {
			notewakeup(&sched.safePointNote)
		}
	}
	if sched.runqsize != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	// If this is the last running P and nobody is polling network,
	// need to wakeup another M to poll network.
	if sched.npidle == uint32(gomaxprocs-1) && atomic.Load64(&sched.lastpoll) != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	if when := nobarrierWakeTime(_p_); when != 0 {
		wakeNetPoller(when)
	}
	pidleput(_p_)
	unlock(&sched.lock)
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
//
//  todo 唤醒 或 新建一个M
//
//	尝试再添加一个P以执行G。
//	当G变为可运行状态（newproc，就绪）时调用。
func wakep() {
	// be conservative about spinning threads
	//
	// 如果有其他的M处于自旋状态，那么就不管了，直接返回
	//
	// 因为自旋的M会拼命找G来运行的，就不新找一个M（劳动者）来运行了
	if !atomic.Cas(&sched.nmspinning, 0, 1) {
		return
	}

	// 启动一个 M  (但是 这是只是一个 M 而已,  还没发起 schedule()调度)
	startm(nil, true)
}

// todo 休眠当前 M
//
// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
//
//
// 停止执行锁定到g的当前m，直到g可再次运行   todo  (该函数 会阻塞)
// 返回获得的 P
func stoplockedm() {
	_g_ := getg()

	if _g_.m.lockedg == 0 || _g_.m.lockedg.ptr().lockedm.ptr() != _g_.m {
		throw("stoplockedm: inconsistent locking")
	}

	// 释放 P
	if _g_.m.p != 0 {
		// Schedule another M to run this p.
		_p_ := releasep()
		handoffp(_p_)
	}
	incidlelocked(1)
	// Wait until another thread schedules lockedg again.
	//
	// 等到另一个线程再次调度 lockedg   (说白了, 就是等待当前 M 被别的 G 释放掉出来)
	notesleep(&_g_.m.park)
	noteclear(&_g_.m.park)
	status := readgstatus(_g_.m.lockedg.ptr())
	if status&^_Gscan != _Grunnable {
		print("runtime:stoplockedm: g is not Grunnable or Gscanrunnable\n")
		dumpgstatus(_g_)
		throw("stoplockedm: not runnable")
	}

	// 去获得 潜在的 P
	acquirep(_g_.m.nextp.ptr())
	_g_.m.nextp = 0
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func startlockedm(gp *g) {
	_g_ := getg()

	mp := gp.lockedm.ptr()
	if mp == _g_.m {
		throw("startlockedm: locked to me")
	}
	if mp.nextp != 0 {
		throw("startlockedm: m has p")
	}
	// directly handoff current P to the locked m
	incidlelocked(-1)
	_p_ := releasep()
	mp.nextp.set(_p_)
	notewakeup(&mp.park)   // 信号灯 唤醒 监听者 M
	stopm()   // ?? 不懂, 为什么 有 停掉 M ??
}


// 停止 当前 M (当 StopTheWorld 来临时)
//
//    该函数 会在  世界重新开始时 才返回
//
// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
func gcstopm() {
	_g_ := getg()

	if sched.gcwaiting == 0 {
		throw("gcstopm: not waiting for gc")
	}
	if _g_.m.spinning {
		_g_.m.spinning = false
		// OK to just drop nmspinning here,
		// startTheWorld will unpark threads as necessary.
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("gcstopm: negative nmspinning")
		}
	}
	_p_ := releasep()   // 释放 M 拥有的 P, P会变为空闲(_Pidle)状态
	lock(&sched.lock)
	_p_.status = _Pgcstop
	sched.stopwait--    //  因为 GC 到来, P 被释放, 故   (还没有停止 的P 的计数  -1)

	// 当 P 都停止了, 向 GC回收器  发出 通知信号
	if sched.stopwait == 0 {
		notewakeup(&sched.stopnote)   // 信号灯 唤醒 监听者M
	}
	unlock(&sched.lock)
	stopm()	// 这里才是 停掉 M
}

// todo 执行 G
//
// Schedules gp to run on the current M.
// If inheritTime is true, gp inherits the remaining time in the
// current time slice. Otherwise, it starts a new time slice.
// Never returns.
//
// Write barriers are allowed because this is called immediately after
// acquiring a P in several places.
//
//go:yeswritebarrierrec
func execute(gp *g, inheritTime bool) {

	// 调用 getg 获取当前的g
	_g_ := getg()

	// Assign gp.m before entering _Grunning so running Gs have an
	// M.
	_g_.m.curg = gp  	// 设置g.m.curg = g
	gp.m = _g_.m		// 设置g.m = m
	casgstatus(gp, _Grunnable, _Grunning)  // 把G的状态由待运行(_Grunnable)改为运行中(_Grunning)
	gp.waitsince = 0
	gp.preempt = false
	gp.stackguard0 = gp.stack.lo + _StackGuard  // 设置G的stackguard, 栈空间不足时可以扩张
	if !inheritTime {
		_g_.m.p.ptr().schedtick++   // 增加P中记录的调度次数(对应上面的每61次优先获取一次全局运行队列)
	}

	// Check whether the profiler needs to be turned on or off.
	hz := sched.profilehz
	if _g_.m.profilehz != hz {
		setThreadCPUProfiler(hz)
	}

	if trace.enabled {
		// GoSysExit has to happen when we have a P, but before GoStart.
		// So we emit it here.
		if gp.syscallsp != 0 && gp.sysblocktraced {
			traceGoSysExit(gp.sysexitticks)
		}
		traceGoStart()
	}

	/**
	todo 有很多 汇编实现
	这个函数会根据 g.sched 中保存的状态恢复各个寄存器的值并继续运行g
		首先针对g.sched.ctxt调用写屏障(GC标记指针存活), ctxt中一般会保存指向[函数+参数]的指针
		设置TLS中的g为g.sched.g, 也就是g自身
		设置rsp寄存器为g.sched.rsp
		设置rax寄存器为g.sched.ret
		设置rdx寄存器为g.sched.ctxt (上下文)
		设置rbp寄存器为g.sched.rbp
		清空sched中保存的信息
		跳转到g.sched.pc
	因为前面创建goroutine的newproc1函数把返回地址设为了goexit, 函数运行完毕返回时将会 调用goexit函数


	g.sched.pc在G首次运行时会指向目标函数的第一条机器指令,
			如果G被抢占或者等待资源而进入休眠, 在休眠前会 【保存状态】 到g.sched,
			g.sched.pc会变为唤醒后需要继续执行的地址

	目标函数执行完毕后会调用goexit函数, goexit函数会调用goexit1函数, goexit1函数会通过mcall调用goexit0函数.
	mcall() 这个函数就是用于实现"保存状态"的
	 */
	gogo(&gp.sched)  // 调用gogo函数
}

// Finds a runnable goroutine to execute.
// Tries to steal from other P's, get g from local or global queue, poll network.
//
// todo 查找  可执行的G
//
// 查找要执行的  可运行goroutine
// 尝试从  其他P 窃取，从 P本地 或 全局队列 获取 G，轮询网络
func findrunnable() (gp *g, inheritTime bool) {
	_g_ := getg()

	// The conditions here and in handoffp must agree: if
	// findrunnable would return a G to run, handoffp must start
	// an M.
	//
	// 这里的条件 和 handoffp()函数 中的条件必须一致：如果 findrunnable()函数 将返回 G 以运行，handoffp()函数 必须以 M 开始

top:
	_p_ := _g_.m.p.ptr()
	if sched.gcwaiting != 0 {
		gcstopm()  // 如果当前GC需要停止整个世界（STW), 则调用 gcstopm 休眠当前的M
		goto top
	}

	// 如果M拥有的P中指定了需要在安全点运行的函数(P.runSafePointFn), 则运行它
	if _p_.runSafePointFn != 0 {
		runSafePointFn()
	}

	// todo 先检查下当前 P 中的 runtimeTimer 堆中的 定时器
	now, pollUntil, _ := checkTimers(_p_, 0)

	if fingwait && fingwake {
		if gp := wakefing(); gp != nil {  // 如果有析构器待运行则使用"运行析构器的G"
			ready(gp, 0, true)  // findrunnable() 快速找一个 G 是被调用
		}
	}
	if *cgo_yield != nil {
		asmcgocall(*cgo_yield, nil)
	}

	// local runq
	if gp, inheritTime := runqget(_p_); gp != nil {   // 从P的本地运行队列中获取G, 调用runqget函数
		return gp, inheritTime
	}

	// global runq
	if sched.runqsize != 0 {
		lock(&sched.lock)
		gp := globrunqget(_p_, 0)  // 从全局运行队列获取G, 调用globrunqget函数, 需要上锁
		unlock(&sched.lock)
		if gp != nil {
			return gp, false
		}
	}

	// Poll network.
	// This netpoll is only an optimization before we resort to stealing.
	// We can safely skip it if there are no waiters or a thread is blocked
	// in netpoll already. If there is any kind of logical race with that
	// blocked thread (e.g. it has already returned from netpoll, but does
	// not set lastpoll yet), this thread will do blocking netpoll below
	// anyway.
	//
	//
	// 从 [网络事件反应器] 获取G,  netpoll()函数  会获取哪些 fd 可读可写 或 已关闭, 然后返回 等待fd 相关事件的 G
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && atomic.Load64(&sched.lastpoll) != 0 {
		if list := netpoll(0); !list.empty() { // non-blocking
			gp := list.pop()
			injectglist(&list)
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			return gp, false
		}
	}

	// Steal work from other P's.
	procs := uint32(gomaxprocs)
	ranTimer := false
	// If number of spinning M's >= number of busy P's, block.
	// This is necessary to prevent excessive CPU consumption
	// when GOMAXPROCS>>1 but the program parallelism is low.
	//
	// 如果当前的M没在自旋 且 正在自旋的M个数的2倍>=正在忙的p的个数时，不让该M进入自旋状态
	//
	// todo  细节
	//
	//		当正在自旋的M的个数的2倍大于等于正在忙的P的个数时，不要让该M自旋。 这是为啥？
	//
	// 		注释上解释说为了避免CPU的过多的消耗.
	// 		毕竟自旋是需要消耗CPU的，如果已经有不少的P处于忙时，应该尽量让P去占用CPU资源，
	// 		而不是为了查找G而浪费CPU. 当然一旦自旋的M找到G后，就会自动退出自旋状态
	if !_g_.m.spinning && 2*atomic.Load(&sched.nmspinning) >= procs-atomic.Load(&sched.npidle) {
		goto stop
	}
	if !_g_.m.spinning {
		_g_.m.spinning = true
		atomic.Xadd(&sched.nmspinning, 1)
	}

	// todo 如果获取不到G, 则执行 Work Stealing  (去其他P的队列中偷来G)
	for i := 0; i < 4; i++ {
		for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
			if sched.gcwaiting != 0 {
				goto top
			}
			stealRunNextG := i > 2 // first look for ready queues with more than 1 g
			p2 := allp[enum.position()]   // todo 从 全局的 P队列中  获取 P的指针
			if _p_ == p2 {
				continue
			}
			// todo 调用runqsteal尝试从其他P的本地运行队列盗取  [ 一半的G]
			if gp := runqsteal(_p_, p2, stealRunNextG); gp != nil {
				return gp, false
			}

			// Consider stealing timers from p2.
			// This call to checkTimers is the only place where
			// we hold a lock on a different P's timers.
			// Lock contention can be a problem here, so avoid
			// grabbing the lock if p2 is running and not marked
			// for preemption. If p2 is running and not being
			// preempted we assume it will handle its own timers.
			if i > 2 && shouldStealTimers(p2) {
				tnow, w, ran := checkTimers(p2, now)   // todo 在 向其他P 偷G的过程中, 也顺便检查下 对应的 P 中的timer 是否可以执行
				now = tnow
				if w != 0 && (pollUntil == 0 || w < pollUntil) {
					pollUntil = w
				}
				if ran {
					// Running the timers may have
					// made an arbitrary number of G's
					// ready and added them to this P's
					// local run queue. That invalidates
					// the assumption of runqsteal
					// that is always has room to add
					// stolen G's. So check now if there
					// is a local G to run.
					if gp, inheritTime := runqget(_p_); gp != nil {
						return gp, inheritTime
					}
					ranTimer = true
				}
			}
		}
	}
	if ranTimer {
		// Running a timer may have made some goroutine ready.
		goto top
	}


// 如果还是获取不到G, 就需要休眠M了, 接下来是休眠的步骤
stop:

	// We have nothing to do. If we're in the GC mark phase, can
	// safely scan and blacken objects, and have work to do, run
	// idle-time marking rather than give up the P.
	//
	// 再次检查当前GC是否在标记阶段, 在则查找有没有待运行的GC Worker, GC Worker也是一个G
	if gcBlackenEnabled != 0 && _p_.gcBgMarkWorker != 0 && gcMarkWorkAvailable(_p_) {
		_p_.gcMarkWorkerMode = gcMarkWorkerIdleMode
		gp := _p_.gcBgMarkWorker.ptr()
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.enabled {
			traceGoUnpark(gp, 0)
		}
		return gp, false
	}

	delta := int64(-1)
	if pollUntil != 0 {
		// checkTimers ensures that polluntil > now.
		delta = pollUntil - now
	}

	// wasm only:
	// If a callback returned and no other goroutine is awake,
	// then pause execution until a callback was triggered.
	if beforeIdle(delta) {
		// At least one goroutine got woken.
		goto top
	}

	// Before we drop our P, make a snapshot of the allp slice,
	// which can change underfoot once we no longer block
	// safe-points. We don't need to snapshot the contents because
	// everything up to cap(allp) is immutable.
	allpSnapshot := allp

	// return P and block
	lock(&sched.lock)

	// 再次检查如果当前GC需要停止整个世界, 或者P指定了需要再安全点运行的函数, 则跳到 findrunnable() 的顶部 <top位置> 重试
	if sched.gcwaiting != 0 || _p_.runSafePointFn != 0 {
		unlock(&sched.lock)
		goto top
	}
	if sched.runqsize != 0 {  // 再次检查  schedt的 全局 runq 队列中是否有G, 有则获取并返回
		gp := globrunqget(_p_, 0)
		unlock(&sched.lock)
		return gp, false
	}

	// 释放M拥有的P, P会变为空闲(_Pidle)状态
	if releasep() != _p_ {
		throw("findrunnable: wrong p")
	}

	// 把P添加到"空闲P链表"中
	pidleput(_p_)
	unlock(&sched.lock)

	// todo 让 M 离开 【自旋状态】, 这里的处理非常重要
	//
	// Delicate dance: thread transitions from spinning to non-spinning state,
	// potentially concurrently with submission of new goroutines. We must
	// drop nmspinning first and then check all per-P queues again (with
	// #StoreLoad memory barrier in between). If we do it the other way around,
	// another thread can submit a goroutine after we've checked all run queues
	// but before we drop nmspinning; as the result nobody will unpark a thread
	// to run the goroutine.
	// If we discover new work below, we need to restore m.spinning as a signal
	// for resetspinning to unpark a new worker thread (because there can be more
	// than one starving goroutine). However, if after discovering new work
	// we also observe no idle Ps, it is OK to just park the current thread:
	// the system is fully loaded so no spinning threads are required.
	// Also see "Worker thread parking/unparking" comment at the top of the file.
	wasSpinning := _g_.m.spinning
	if _g_.m.spinning {
		_g_.m.spinning = false

		// 首先减少表示当前 【自旋】 中的M的数量的全局变量 nmspinning
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("findrunnable: negative nmspinning")
		}
	}

	// check all runqueues once again
	for _, _p_ := range allpSnapshot {

		// 再次检查所有 P 的本地 runq队列, 如果不为空则让M重新进入 【自旋状态】, 并跳到 findrunnable() 的顶部 <top位置> 重试
		if !runqempty(_p_) {
			lock(&sched.lock)
			_p_ = pidleget()
			unlock(&sched.lock)
			if _p_ != nil {
				acquirep(_p_)
				if wasSpinning {
					_g_.m.spinning = true
					atomic.Xadd(&sched.nmspinning, 1)
				}
				goto top
			}
			break
		}
	}

	// Check for idle-priority GC work again.
	// 再次检查有没有待运行的GC Worker, 有则让M重新进入 【自旋状态】, 并跳到findrunnable的顶部重试
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(nil) {
		lock(&sched.lock)
		_p_ = pidleget()
		if _p_ != nil && _p_.gcBgMarkWorker == 0 {
			pidleput(_p_)
			_p_ = nil
		}
		unlock(&sched.lock)
		if _p_ != nil {
			acquirep(_p_)
			if wasSpinning {
				_g_.m.spinning = true
				atomic.Xadd(&sched.nmspinning, 1)
			}
			// Go back to idle GC check.
			goto stop
		}
	}

	// poll network
	// 再次检查  网络事件反应器  是否有待运行的G, 这里对netpoll的调用会阻塞, 直到某个fd收到了事件
	if netpollinited() && (atomic.Load(&netpollWaiters) > 0 || pollUntil != 0) && atomic.Xchg64(&sched.lastpoll, 0) != 0 {
		atomic.Store64(&sched.pollUntil, uint64(pollUntil))
		if _g_.m.p != 0 {
			throw("findrunnable: netpoll with p")
		}
		if _g_.m.spinning {
			throw("findrunnable: netpoll with spinning")
		}
		if faketime != 0 {
			// When using fake time, just poll.
			delta = 0
		}
		list := netpoll(delta) // block until new work is available
		atomic.Store64(&sched.pollUntil, 0)
		atomic.Store64(&sched.lastpoll, uint64(nanotime()))
		if faketime != 0 && list.empty() {
			// Using fake time and nothing is ready; stop M.
			// When all M's stop, checkdead will call timejump.
			stopm()
			goto top
		}
		lock(&sched.lock)
		_p_ = pidleget()
		unlock(&sched.lock)
		if _p_ == nil {
			injectglist(&list)
		} else {
			acquirep(_p_)
			if !list.empty() {
				gp := list.pop()
				injectglist(&list)
				casgstatus(gp, _Gwaiting, _Grunnable)
				if trace.enabled {
					traceGoUnpark(gp, 0)
				}
				return gp, false
			}
			if wasSpinning {
				_g_.m.spinning = true
				atomic.Xadd(&sched.nmspinning, 1)
			}
			goto top
		}
	} else if pollUntil != 0 && netpollinited() {
		pollerPollUntil := int64(atomic.Load64(&sched.pollUntil))
		if pollerPollUntil == 0 || pollerPollUntil > pollUntil {
			netpollBreak()
		}
	}

	// 如果 最终还是获取不到G, 调用stopm休眠当前的M
	stopm()
	goto top   // 唤醒M 后跳到 findrunnable() 的 顶部 <top位置> 重试
}

// pollWork reports whether there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
func pollWork() bool {
	if sched.runqsize != 0 {
		return true
	}
	p := getg().m.p.ptr()
	if !runqempty(p) {
		return true
	}
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && sched.lastpoll != 0 {
		if list := netpoll(0); !list.empty() {
			injectglist(&list)
			return true
		}
	}
	return false
}

// wakeNetPoller wakes up the thread sleeping in the network poller,
// if there is one, and if it isn't going to wake up anyhow before
// the when argument.
func wakeNetPoller(when int64) {
	if atomic.Load64(&sched.lastpoll) == 0 {
		// In findrunnable we ensure that when polling the pollUntil
		// field is either zero or the time to which the current
		// poll is expected to run. This can have a spurious wakeup
		// but should never miss a wakeup.
		pollerPollUntil := int64(atomic.Load64(&sched.pollUntil))
		if pollerPollUntil == 0 || pollerPollUntil > when {
			netpollBreak()  // 唤醒网络轮询器，例如：计时器向前修改时间时会通过该函数中断网络轮询器
		}
	}
}

func resetspinning() {
	_g_ := getg()
	if !_g_.m.spinning {
		throw("resetspinning: not a spinning m")
	}
	_g_.m.spinning = false
	nmspinning := atomic.Xadd(&sched.nmspinning, -1)
	if int32(nmspinning) < 0 {
		throw("findrunnable: negative nmspinning")
	}
	// M wakeup policy is deliberately somewhat conservative, so check if we
	// need to wakeup another P here. See "Worker thread parking/unparking"
	// comment at the top of the file for details.
	if nmspinning == 0 && atomic.Load(&sched.npidle) > 0 {   // 如果当前有空闲的P, 但是无自旋的M(nmspinning等于0), 则唤醒或新建一个M
		wakep()
	}
}

// Injects the list of runnable G's into the scheduler and clears glist.
// Can run concurrently with GC.
func injectglist(glist *gList) {
	if glist.empty() {
		return
	}
	if trace.enabled {
		for gp := glist.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
			traceGoUnpark(gp, 0)
		}
	}
	lock(&sched.lock)
	var n int
	for n = 0; !glist.empty(); n++ {
		gp := glist.pop()
		casgstatus(gp, _Gwaiting, _Grunnable)
		globrunqput(gp)
	}
	unlock(&sched.lock)
	for ; n != 0 && sched.npidle != 0; n-- {
		startm(nil, false)
	}
	*glist = gList{}
}

// One round of scheduler: find a runnable goroutine and execute it.
// Never returns.
//
//
// go中有名的 todo 【一轮调度程序】：找到一个可运行的goroutine并执行它
//
// 永不返回
//
//  schedule函数获取g => [必要时休眠] => [唤醒后继续获取] => execute 函数执行g => 执行后返回到goexit => 重新执行schedule函数
//
//      最终我们看到的是：  schedule() -> ... -> goexit0() -> schedule() -> ... ->  goexit0() -> ... 这样纸 一轮一轮的调度
//
// 不仅只有G结束会重新开始调度, G被抢占 <sysmon()函数实现抢占>  或者等待资源也会重新进行调度
func schedule() {

	// 最开始的时候, 这个是 g0
	_g_ := getg()

	if _g_.m.locks != 0 {
		throw("schedule: holding locks")
	}

	// todo 如果当前 GC 需要 停止整个世界（STW), 则调用 stopm 休眠 当前M
	if _g_.m.lockedg != 0 {
		// 休眠 当前 M
		// 停止执行锁定到g的当前m，直到g可再次运行   todo  (该函数 会阻塞)
		stoplockedm()
		// 执行被锁定的 G
		execute(_g_.m.lockedg.ptr(), false) // Never returns.
	}

	// We should not schedule away from a g that is executing a cgo call,
	// since the cgo call is using the m's g0 stack.
	//
	// 由于执行 cgo调用 使用了 m的g0堆栈， 因此我们 不应 将正在执行 cgo调用的 G 安排在远离的地方
	if _g_.m.incgo {
		throw("schedule: in cgo")
	}

top:
	pp := _g_.m.p.ptr()
	pp.preempt = false


	// todo 如果正在 gc, 那么 当前 调度是 停止 P 和 M 的
	if sched.gcwaiting != 0 {
		gcstopm()   // 停止 当前 M (当 StopTheWorld 来临时)
		goto top
	}

	// 如果 M 拥有的 P 中指定了需要在 安全点运行的函数 (P.runSafePointFn), 则运行它
	if pp.runSafePointFn != 0 {
		runSafePointFn()
	}

	// Sanity check: if we are spinning, the run queue should be empty.
	// Check this before calling checkTimers, as that might call
	// goready to put a ready goroutine on the local run queue.
	if _g_.m.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
		throw("schedule: spinning with local work")
	}


	// todo 检查下 P 中的定时器是否可以 执行
	checkTimers(pp, 0)

	var gp *g    // todo 用来接收 即将从 队列中获取的 新G 去运行
	var inheritTime bool

	// Normal goroutines will check for need to wakeP in ready,
	// but GCworkers and tracereaders will not, so the check must
	// be done here instead.
	//
	// 普通的 goroutine 会检查是否需要 readyWakeP，但是 GCworkers 和 tracereader 不会，因此必须在此处进行检查
	tryWakeP := false
	if trace.enabled || trace.shutdown {
		gp = traceReader()  // 快速获取待运行的G, 以下处理如果有一个获取成功后面就不会继续获取
		if gp != nil {
			casgstatus(gp, _Gwaiting, _Grunnable)
			traceGoUnpark(gp, 0)
			tryWakeP = true
		}
	}

	// 如果当前GC正在 标记阶段 (mark), 则查找 有没有待运行的 GC Worker, GC Worker也是一个G
	if gp == nil && gcBlackenEnabled != 0 {
		gp = gcController.findRunnableGCWorker(_g_.m.p.ptr())  // todo 在 【并行 gc】 中, 后台为 每个P启动了一个后台标记任务, 但是可以同时工作的只有25%, 这个逻辑在协程 M 获取 G 时调用
		tryWakeP = tryWakeP || gp != nil
	}
	if gp == nil {
		// Check the global runnable queue once in a while to ensure fairness.
		// Otherwise two goroutines can completely occupy the local runqueue
		// by constantly respawning each other.
		//
		//
		// 偶尔检查 schedt 的全局 runq 队列以确保公平性
		// 否则，两个 goroutine 可以通过不断地彼此重生来完全占据本地运行队列
		if _g_.m.p.ptr().schedtick%61 == 0 && sched.runqsize > 0 {    // 为了公平起见, 每 61 次调度从 schedt 的 全局 runq队列获取一次G, (一直从 P本地获取可能导致全局运行队列中的G不被运行)
			lock(&sched.lock)
			gp = globrunqget(_g_.m.p.ptr(), 1)
			unlock(&sched.lock)
		}
	}
	if gp == nil {
		gp, inheritTime = runqget(_g_.m.p.ptr())  // todo 从P的本地运行队列中获取G, 调用runqget函数
		// We can see gp != nil here even if the M is spinning,
		// if checkTimers added a local goroutine via goready.
	}
	if gp == nil {
		// todo 快速获取G 失败时, 调用 findrunnable() 函数 获取待运行的G, 会阻塞到获取成功为止  【重要】
		//
		// 尝试从  其他P 窃取，从 P本地 或 全局队列 获取 G，轮询网络
		gp, inheritTime = findrunnable() // blocks until work is available
	}

	// 成功获取到一个待运行的G

	// This thread is going to run a goroutine and is not spinning anymore,
	// so if it was marked as spinning we need to reset it now and potentially
	// start a new spinning M.
	if _g_.m.spinning {
		resetspinning()   // 让 M 离开 【自旋状态】, 调用 resetspinning() 函数, 这里的处理 和 上面 [findrunnable()] 的不一样
		// 上面离开 【自旋状态】 是为了  休眠M, 所以会再次检查所有队列然后休眠
		// 这里离开 【自旋状态】 是为了  执行G, 所以会检查是否有空闲的P, 有则表示可以再开新的M执行G
	}

	if sched.disable.user && !schedEnabled(gp) {
		// Scheduling of this goroutine is disabled. Put it on
		// the list of pending runnable goroutines for when we
		// re-enable user scheduling and look again.
		lock(&sched.lock)
		if schedEnabled(gp) {
			// Something re-enabled scheduling while we
			// were acquiring the lock.
			unlock(&sched.lock)
		} else {
			sched.disable.runnable.pushBack(gp)
			sched.disable.n++
			unlock(&sched.lock)
			goto top
		}
	}

	// If about to schedule a not-normal goroutine (a GCworker or tracereader),
	// wake a P if there is one.
	if tryWakeP {
		if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
			wakep()
		}
	}

	// 如果G要求回到指定的M(例如上面的runtime.main)
	if gp.lockedm != 0 {
		// Hands off own p to the locked m,
		// then blocks waiting for a new p.
		//
		// 将自己的 P 交给 锁定的 M，然后 阻止等待新的 P
		//
		startlockedm(gp)  	// 调用 startlockedm()函数 把 G 和 P 交给 该M, 自己进入休眠
		goto top			// 从休眠唤醒后跳到 schedule() 的 顶部重试
	}

	execute(gp, inheritTime)  // todo 调用execute函数 执行G
}

// 解除 M 和 G 之间的关联
//
//
// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
func dropg() {
	_g_ := getg()

	setMNoWB(&_g_.m.curg.m, nil)
	setGNoWB(&_g_.m.curg, nil)
}

// TODO 当前 P 检查Timer 堆中的 Timer
//
// checkTimers runs any timers for the P that are ready.
// If now is not 0 it is the current time.
// It returns the current time or 0 if it is not known,
// and the time when the next timer should run or 0 if there is no next timer,
// and reports whether it ran any timers.
// If the time when the next timer should run is not 0,
// it is always larger than the returned time.
// We pass now in and out to avoid extra calls of nanotime.
//
//
// `checkTimers()`  为已运行的P运行任何计时器 
//					如果现在不为0，则为当前时间
//
//					它返回当前时间，如果未知，则返回0，如果不知道下一个定时器应运行的时间，则返回0；
// 					如果不存在下一个定时器，则返回0，并报告是否运行了任何定时器
//					如果下一个计时器应运行的时间不为0，则始终大于返回的时间 
//					我们现在传入和传出，以避免额外调用nanotime 
//
//
// 该方法在 三个地方 中 被优先调用:
//
//		schedule():		启动一轮调度的开始时
//		findrunnable(): 快速获取一个 可执行的G 的开始时
//		findrunnable(): 快速获取一个 可执行的G 的从其他P中偷G动作时
//
//go:yeswritebarrierrec
func checkTimers(pp *p, now int64) (rnow, pollUntil int64, ran bool) {
	// If there are no timers to adjust, and the first timer on
	// the heap is not yet ready to run, then there is nothing to do.
	if atomic.Load(&pp.adjustTimers) == 0 {
		next := int64(atomic.Load64(&pp.timer0When))
		if next == 0 {
			return now, 0, false
		}
		if now == 0 {
			now = nanotime()
		}
		if now < next {
			// Next timer is not ready to run.
			// But keep going if we would clear deleted timers.
			// This corresponds to the condition below where
			// we decide whether to call clearDeletedTimers.
			if pp != getg().m.p.ptr() || int(atomic.Load(&pp.deletedTimers)) <= int(atomic.Load(&pp.numTimers)/4) {
				return now, next, false
			}
		}
	}

	lock(&pp.timersLock)

	adjusttimers(pp)

	rnow = now
	if len(pp.timers) > 0 {
		if rnow == 0 {
			rnow = nanotime()
		}
		for len(pp.timers) > 0 {
			// Note that runtimer may temporarily unlock
			// pp.timersLock.
			if tw := runtimer(pp, rnow); tw != 0 {    // checkTimers() 中, 执行 定时器 (调度定时器)
				if tw > 0 {
					pollUntil = tw
				}
				break
			}
			ran = true
		}
	}

	// If this is the local P, and there are a lot of deleted timers,
	// clear them out. We only do this for the local P to reduce
	// lock contention on timersLock.
	if pp == getg().m.p.ptr() && int(atomic.Load(&pp.deletedTimers)) > len(pp.timers)/4 {
		clearDeletedTimers(pp)
	}

	unlock(&pp.timersLock)

	return rnow, pollUntil, ran
}

// shouldStealTimers reports whether we should try stealing the timers from p2.
// We don't steal timers from a running P that is not marked for preemption,
// on the assumption that it will run its own timers. This reduces
// contention on the timers lock.
func shouldStealTimers(p2 *p) bool {
	if p2.status != _Prunning {
		return true
	}
	mp := p2.m.ptr()
	if mp == nil || mp.locks > 0 {
		return false
	}
	gp := mp.curg
	if gp == nil || gp.atomicstatus != _Grunning || !gp.preempt {
		return false
	}
	return true
}

func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	return true
}

// park continuation on g0.
//
// 在 g0 上继续 park
//
// todo 停止当前 M 的G， 使用 M 去运行 其他 G
//
// park_m 执行之后，调度器就调度并执行其他的 g， 之前的 gp 也就等待了
func park_m(gp *g) {  // 只有 gopark() 会调用

	// 当前 g 是 g0
	_g_ := getg()

	if trace.enabled {
		traceGoPark(_g_.m.waittraceev, _g_.m.waittraceskip)
	}

	// 把G的状态从运行中(_Grunning)改为等待中(_Gwaiting)   todo 这时候的 g 是 【游离态的】, 只有当在被 调用 goready() 是才会将状态改为 _Grunnable 并放回 p 的 可运行g队列中
	//
	// g 为什么是 游离态的 ?? 因为 g 之前是从 p 的 可运行g队列中 取出来的.
	casgstatus(gp, _Grunning, _Gwaiting)

	dropg() // 解除 g 和 m 的关系

	if fn := _g_.m.waitunlockf; fn != nil {
		// 执行解锁操作， 假如是从 sema 过来的，fn 永远返回 true
		ok := fn(gp, _g_.m.waitlock)  // 再调用传入的解锁函数, 这里的解锁函数会对解除 channel.lock 的锁定
		_g_.m.waitunlockf = nil
		_g_.m.waitlock = nil
		if !ok {
			if trace.enabled {
				traceGoUnpark(gp, 2)
			}
			casgstatus(gp, _Gwaiting, _Grunnable)
			execute(gp, true) // Schedule it back, never returns.
		}
	}

	schedule()  // todo 在 park_m() 中 (停止当前 M 的G， 使用 M 去运行 其他 G),  调用 schedule() 发起新的【一轮调度】 (停止当前 M 的G， 使用 M 去运行 其他 G)
}

// 最终最终抢占的 最最底层实现函数
//
//
//	函数会:
//			把G的状态由运行中(_Grunnable)改为待运行(_Grunnable)
//			调用dropg函数解除M和G之间的关联
//			调用globrunqput把G放到全局运行队列
//			调用schedule函数继续调度
func goschedImpl(gp *g) {
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	casgstatus(gp, _Grunning, _Grunnable) 	// 把G的状态由运行中(_Grunnable)改为待运行(_Grunnable)
	dropg()									// 调用dropg函数解除M和G之间的关联
	lock(&sched.lock)
	globrunqput(gp)							// 调用globrunqput把G放到 schedt的 全局 runq队列
	unlock(&sched.lock)

	schedule() // todo 在 goschedImpl() 中 让出当前 G,  让 M 去执行其他 G, 触发起 新的一轮调度 【一轮调度】

	/**
	因为 schedt 的 全局 runq 队列的优先度比较低, 各个M会经过一段时间再去重新获取这个G执行,
	todo 【抢占机制】保证了不会有一个G长时间的运行导致其他G无法运行的情况发生
	 */
}

// Gosched continuation on g0.   g0 上的 Gosched() 连续
func gosched_m(gp *g) {
	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m
func goschedguarded_m(gp *g) {

	if !canPreemptM(gp.m) {
		gogo(&gp.sched) // never return
	}

	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// 给G抢占一个M
//
// 函数会,
func gopreempt_m(gp *g) {
	if trace.enabled {
		traceGoPreempt()
	}
	goschedImpl(gp)  // 调用goschedImpl函数
}

// preemptPark parks gp and puts it in _Gpreempted.   preemptPark() 停掉 G 并将其放入_Gpreempted
//
//go:systemstack
func preemptPark(gp *g) {
	if trace.enabled {
		traceGoPark(traceEvGoBlock, 0)
	}
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	gp.waitreason = waitReasonPreempted
	// Transition from _Grunning to _Gscan|_Gpreempted. We can't
	// be in _Grunning when we dropg because then we'd be running
	// without an M, but the moment we're in _Gpreempted,
	// something could claim this G before we've fully cleaned it
	// up. Hence, we set the scan bit to lock down further
	// transitions until we can dropg.
	casGToPreemptScan(gp, _Grunning, _Gscan|_Gpreempted)
	dropg()
	casfrom_Gscanstatus(gp, _Gscan|_Gpreempted, _Gpreempted)
	schedule()   // todo 在 preemptPark() 中,  停掉 G 并将其放入_Gpreempted， 并发起新的 【一轮调度】
}


// todo 切换 M 中的 G, 发起新的【一轮调度】
// goyield is like Gosched, but it:
// - emits a GoPreempt trace event instead of a GoSched trace event
// - puts the current G on the runq of the current P instead of the globrunq
//
// goyield 类似于Gosched，但它：
//		- 发出 `GoPreempt` 跟踪事件，而不是 `GoSched` 跟踪事件
//		- 将当前G 放在当前P 的 runq上，而不是 globrunq
//
// goyield 调用 mcall 执行 goyield_m， goyield_m 会把当前的 g 放到 p 本地对象的队尾， 然后执行调度器 发起新的【一轮调度】
func goyield() {
	checkTimeouts()
	mcall(goyield_m)
}
// goyield_m 会把当前的 g 放到 p 本地对象的队尾， 然后执行调度器 发起新的【一轮调度】
func goyield_m(gp *g) {
	if trace.enabled {
		traceGoPreempt()
	}
	pp := gp.m.p.ptr()
	casgstatus(gp, _Grunning, _Grunnable)
	dropg()   // 解除 M 和 G 之间的关联
	runqput(pp, gp, false)   // 将 G 放到 P 的 runq 中
	schedule()  // todo 在 goyield_m() 中 把当前的 g 放到 p 本地对象的队尾， 然后执行调度器 发起新的【一轮调度】
}

// Finishes execution of the current goroutine.
func goexit1() {
	if raceenabled {
		racegoend()
	}
	if trace.enabled {
		traceGoEnd()
	}
	mcall(goexit0)  // 这个函数就是用于实现"保存状态"的
	/**
	todo mcal()函数 做的事情如下:
		设置g.sched.pc等于当前的返回地址
		设置g.sched.sp等于寄存器rsp的值
		设置g.sched.g等于当前的g
		设置g.sched.bp等于寄存器rbp的值
		切换TLS中当前的g等于m.g0
		设置寄存器rsp等于g0.sched.sp, 使用g0的栈空间
		设置第一个参数为原来的g
		设置rdx寄存器为指向函数地址的指针(上下文)
		调用指定的函数, 不会返回


	mcall这个函数保存当前的运行状态到g.sched, 然后切换到g0和g0的栈空间, 再调用指定的函数.
			回到g0的栈空间这个步骤非常重要, 因为这个时候g已经中断, 继续使用g的栈空间且其他M唤醒了这个g将会产生灾难性的后果.
			G在中断或者结束后都会通过mcall回到g0的栈空间继续调度, 从goexit调用的mcall的保存状态其实是多余的, 因为G已经结束了
	 */
}

// goexit continuation on g0.   // g0 上的 goexit() 延续
func goexit0(gp *g) {

	/**
	 goexit0函数调用时已经回到了g0的栈空间, 处理如下:

		把G的状态由运行中(_Grunning)改为已中止(_Gdead)
		清空G的成员
		调用dropg函数解除M和G之间的关联
		调用gfput函数把G放到P的自由列表中, 下次创建G时可以复用
		调用schedule函数继续调度
	 */

	_g_ := getg()

	casgstatus(gp, _Grunning, _Gdead)  // todo 把G的状态由运行中(_Grunning)改为已中止(_Gdead)
	if isSystemGoroutine(gp, false) {
		atomic.Xadd(&sched.ngsys, -1)
	}

	// 清空G的成员
	gp.m = nil
	locked := gp.lockedm != 0
	gp.lockedm = 0
	_g_.m.lockedg = 0
	gp.preemptStop = false
	gp.paniconfault = false
	gp._defer = nil // should be true already but just in case.
	gp._panic = nil // non-nil for Goexit during panic. points at stack-allocated data.
	gp.writebuf = nil
	gp.waitreason = 0
	gp.param = nil
	gp.labels = nil
	gp.timer = nil

	if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
		// Flush assist credit to the global pool. This gives
		// better information to pacing if the application is
		// rapidly creating an exiting goroutines.
		scanCredit := int64(gcController.assistWorkPerByte * float64(gp.gcAssistBytes))
		atomic.Xaddint64(&gcController.bgScanCredit, scanCredit)
		gp.gcAssistBytes = 0
	}

	// 调用dropg函数  解除 M 和 G 之间的关联
	dropg()

	if GOARCH == "wasm" { // no threads yet on wasm
		gfput(_g_.m.p.ptr(), gp)  		// 调用gfput函数 把G放到P的自由列表中, 下次创建G时可以复用
		schedule() // never returns  	 todo 在 goexit0() 中, 执行完当前 G了, 发起新一轮 【一轮调度】
	}

	if _g_.m.lockedInt != 0 {
		print("invalid m->lockedInt = ", _g_.m.lockedInt, "\n")
		throw("internal lockOSThread error")
	}
	gfput(_g_.m.p.ptr(), gp)  // 调用gfput函数 把G放到P的自由列表中, 下次创建G时可以复用
	if locked {
		// The goroutine may have locked this thread because
		// it put it in an unusual kernel state. Kill it
		// rather than returning it to the thread pool.

		// Return to mstart, which will release the P and exit
		// the thread.
		if GOOS != "plan9" { // See golang.org/issue/22227.
			gogo(&_g_.m.g0.sched)
		} else {
			// Clear lockedExt on plan9 since we may end up re-using
			// this thread.
			_g_.m.lockedExt = 0
		}
	}
	schedule()  // todo 在 goexit0() 中, 执行完当前 G了, 发起新一轮 【一轮调度】   (G结束后回到schedule函数, 这样就结束了一个调度循环  最终我们看到的是：  schedule() -> ... -> goexit0() -> schedule() -> ... ->  goexit0() -> ... 这样纸 一轮一轮的调度)
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
//
//go:nosplit
//go:nowritebarrierrec
func save(pc, sp uintptr) {
	_g_ := getg()

	_g_.sched.pc = pc
	_g_.sched.sp = sp
	_g_.sched.lr = 0
	_g_.sched.ret = 0
	_g_.sched.g = guintptr(unsafe.Pointer(_g_))
	// We need to ensure ctxt is zero, but can't have a write
	// barrier here. However, it should always already be zero.
	// Assert that.
	if _g_.sched.ctxt != nil {
		badctxt()
	}
}

// The goroutine g is about to enter a system call.
// Record that it's not using the cpu anymore.
// This is called only from the go syscall library and cgocall,
// not from the low-level system calls used by the runtime.
//
// Entersyscall cannot split the stack: the gosave must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
//
// Nothing entersyscall calls can split the stack either.
// We cannot safely move the stack during an active call to syscall,
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack).
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
//
// Syscall tracing:
// At the start of a syscall we emit traceGoSysCall to capture the stack trace.
// If the syscall does not block, that is it, we do not emit any other events.
// If the syscall blocks (that is, P is retaken), retaker emits traceGoSysBlock;
// when syscall returns we emit traceGoSysExit and when the goroutine starts running
// (potentially instantly, if exitsyscallfast returns true) we emit traceGoStart.
// To ensure that traceGoSysExit is emitted strictly after traceGoSysBlock,
// we remember current value of syscalltick in m (_g_.m.syscalltick = _g_.m.p.ptr().syscalltick),
// whoever emits traceGoSysBlock increments p.syscalltick afterwards;
// and we wait for the increment before emitting traceGoSysExit.
// Note that the increment is done even if tracing is not enabled,
// because tracing can be enabled in the middle of syscall. We don't want the wait to hang.
//
//go:nosplit
func reentersyscall(pc, sp uintptr) {
	_g_ := getg()

	// Disable preemption because during this function g is in Gsyscall status,
	// but can have inconsistent g->sched, do not let GC observe it.
	_g_.m.locks++

	// Entersyscall must not call any function that might split/grow the stack.
	// (See details in comment above.)
	// Catch calls that might, by replacing the stack guard with something that
	// will trip any stack check and leaving a flag to tell newstack to die.
	_g_.stackguard0 = stackPreempt
	_g_.throwsplit = true

	// Leave SP around for GC and traceback.
	save(pc, sp)
	_g_.syscallsp = sp
	_g_.syscallpc = pc
	casgstatus(_g_, _Grunning, _Gsyscall)
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscall inconsistent ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscall")
		})
	}

	if trace.enabled {
		systemstack(traceGoSysCall)
		// systemstack itself clobbers g.sched.{pc,sp} and we might
		// need them later when the G is genuinely blocked in a
		// syscall
		save(pc, sp)
	}

	if atomic.Load(&sched.sysmonwait) != 0 {
		systemstack(entersyscall_sysmon)
		save(pc, sp)
	}

	if _g_.m.p.ptr().runSafePointFn != 0 {
		// runSafePointFn may stack split if run on this stack
		systemstack(runSafePointFn)
		save(pc, sp)
	}

	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	_g_.m.mcache = nil
	pp := _g_.m.p.ptr()
	pp.m = 0
	_g_.m.oldp.set(pp)   // M 要开始做 【系统调用】 了, 将 P 解除, 并 记录P 到 oldp字段, 在从 【系统调用】 中恢复时, 会优先 尝试 重新和这个P绑定
	_g_.m.p = 0
	atomic.Store(&pp.status, _Psyscall)
	if sched.gcwaiting != 0 {
		systemstack(entersyscall_gcwait)
		save(pc, sp)
	}

	_g_.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
//
// This is exported via linkname to assembly in the syscall package.
//
//go:nosplit
//go:linkname entersyscall
func entersyscall() {
	reentersyscall(getcallerpc(), getcallersp())
}

func entersyscall_sysmon() {
	lock(&sched.lock)
	if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
}

func entersyscall_gcwait() {
	_g_ := getg()
	_p_ := _g_.m.oldp.ptr()

	lock(&sched.lock)
	if sched.stopwait > 0 && atomic.Cas(&_p_.status, _Psyscall, _Pgcstop) {
		if trace.enabled {
			traceGoSysBlock(_p_)
			traceProcStop(_p_)
		}
		_p_.syscalltick++
		if sched.stopwait--; sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
	}
	unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.
//go:nosplit
func entersyscallblock() {
	_g_ := getg()

	_g_.m.locks++ // see comment in entersyscall
	_g_.throwsplit = true
	_g_.stackguard0 = stackPreempt // see comment in entersyscall
	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	_g_.m.p.ptr().syscalltick++

	// Leave SP around for GC and traceback.
	pc := getcallerpc()
	sp := getcallersp()
	save(pc, sp)
	_g_.syscallsp = _g_.sched.sp
	_g_.syscallpc = _g_.sched.pc
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		sp1 := sp
		sp2 := _g_.sched.sp
		sp3 := _g_.syscallsp
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	casgstatus(_g_, _Grunning, _Gsyscall)
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp), " ", hex(_g_.sched.sp), " ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}

	systemstack(entersyscallblock_handoff)

	// Resave for traceback during blocked call.
	save(getcallerpc(), getcallersp())

	_g_.m.locks--
}

func entersyscallblock_handoff() {
	if trace.enabled {
		traceGoSysCall()
		traceGoSysBlock(getg().m.p.ptr())
	}
	handoffp(releasep())
}

// 将当前G 退出 【系统调度用】 的对外实现
//
// The goroutine g exited its system call.
// Arrange for it to run on a cpu again.
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
//
// Write barriers are not allowed because our P may have been stolen.
//
// This is exported via linkname to assembly in the syscall package.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname exitsyscall
func exitsyscall() {
	_g_ := getg()

	_g_.m.locks++ // see comment in entersyscall
	if getcallersp() > _g_.syscallsp {
		throw("exitsyscall: syscall frame is no longer valid")
	}

	_g_.waitsince = 0
	oldp := _g_.m.oldp.ptr()
	_g_.m.oldp = 0
	if exitsyscallfast(oldp) {
		if _g_.m.mcache == nil {
			throw("lost mcache")
		}
		if trace.enabled {
			if oldp != _g_.m.p.ptr() || _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
				systemstack(traceGoStart)
			}
		}
		// There's a cpu for us, so we can run.
		_g_.m.p.ptr().syscalltick++
		// We need to cas the status and scan before resuming...
		casgstatus(_g_, _Gsyscall, _Grunning)

		// Garbage collector isn't running (since we are),
		// so okay to clear syscallsp.
		_g_.syscallsp = 0
		_g_.m.locks--
		if _g_.preempt {
			// restore the preemption request in case we've cleared it in newstack
			_g_.stackguard0 = stackPreempt
		} else {
			// otherwise restore the real _StackGuard, we've spoiled it in entersyscall/entersyscallblock
			_g_.stackguard0 = _g_.stack.lo + _StackGuard
		}
		_g_.throwsplit = false

		if sched.disable.user && !schedEnabled(_g_) {
			// Scheduling of this goroutine is disabled.
			Gosched()
		}

		return
	}

	_g_.sysexitticks = 0
	if trace.enabled {
		// Wait till traceGoSysBlock event is emitted.
		// This ensures consistency of the trace (the goroutine is started after it is blocked).
		for oldp != nil && oldp.syscalltick == _g_.m.syscalltick {
			osyield()
		}
		// We can't trace syscall exit right now because we don't have a P.
		// Tracing code can invoke write barriers that cannot run without a P.
		// So instead we remember the syscall exit time and emit the event
		// in execute when we have a P.
		_g_.sysexitticks = cputicks()
	}

	_g_.m.locks--

	// Call the scheduler.
	mcall(exitsyscall0)

	if _g_.m.mcache == nil {
		throw("lost mcache")
	}

	// Scheduler returned, so we're allowed to run now.
	// Delete the syscallsp information that we left for
	// the garbage collector during the system call.
	// Must wait until now because until gosched returns
	// we don't know for sure that the garbage collector
	// is not running.
	_g_.syscallsp = 0
	_g_.m.p.ptr().syscalltick++
	_g_.throwsplit = false
}

//go:nosplit
func exitsyscallfast(oldp *p) bool {
	_g_ := getg()

	// Freezetheworld sets stopwait but does not retake P's.
	if sched.stopwait == freezeStopWait {
		return false
	}

	// Try to re-acquire the last P.
	if oldp != nil && oldp.status == _Psyscall && atomic.Cas(&oldp.status, _Psyscall, _Pidle) {
		// There's a cpu for us, so we can run.
		wirep(oldp)
		exitsyscallfast_reacquired()
		return true
	}

	// Try to get any other idle P.
	if sched.pidle != 0 {
		var ok bool
		systemstack(func() {
			ok = exitsyscallfast_pidle()
			if ok && trace.enabled {
				if oldp != nil {
					// Wait till traceGoSysBlock event is emitted.
					// This ensures consistency of the trace (the goroutine is started after it is blocked).
					for oldp.syscalltick == _g_.m.syscalltick {
						osyield()
					}
				}
				traceGoSysExit(0)
			}
		})
		if ok {
			return true
		}
	}
	return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
//
//go:nosplit
func exitsyscallfast_reacquired() {
	_g_ := getg()
	if _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
		if trace.enabled {
			// The p was retaken and then enter into syscall again (since _g_.m.syscalltick has changed).
			// traceGoSysBlock for this syscall was already emitted,
			// but here we effectively retake the p from the new syscall running on the same p.
			systemstack(func() {
				// Denote blocking of the new syscall.
				traceGoSysBlock(_g_.m.p.ptr())
				// Denote completion of the current syscall.
				traceGoSysExit(0)
			})
		}
		_g_.m.p.ptr().syscalltick++
	}
}

func exitsyscallfast_pidle() bool {
	lock(&sched.lock)
	_p_ := pidleget()
	if _p_ != nil && atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		acquirep(_p_)
		return true
	}
	return false
}

// 将当前G 退出 【系统调度用】 的底层真正实现
//
// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
	_g_ := getg()

	casgstatus(gp, _Gsyscall, _Grunnable)
	dropg()
	lock(&sched.lock)
	var _p_ *p
	if schedEnabled(_g_) {
		_p_ = pidleget()
	}
	if _p_ == nil {
		globrunqput(gp)
	} else if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		acquirep(_p_)
		execute(gp, false) // Never returns.
	}
	if _g_.m.lockedg != 0 {
		// Wait until another thread schedules gp and so m again.
		stoplockedm()
		execute(gp, false) // Never returns.
	}
	stopm()
	schedule() // Never returns.  todo  在 exitsyscall0() 中, 当前 G 退出 【系统调用】, 并发起新的【一轮调度】
}

func beforefork() {
	gp := getg().m.curg

	// Block signals during a fork, so that the child does not run
	// a signal handler before exec if a signal is sent to the process
	// group. See issue #18600.
	gp.m.locks++
	msigsave(gp.m)
	sigblock()

	// This function is called before fork in syscall package.
	// Code between fork and exec must not allocate memory nor even try to grow stack.
	// Here we spoil g->_StackGuard to reliably detect any attempts to grow stack.
	// runtime_AfterFork will undo this in parent process, but not in child.
	gp.stackguard0 = stackFork
}

// Called from syscall package before fork.
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
	systemstack(beforefork)
}

func afterfork() {
	gp := getg().m.curg

	// See the comments in beforefork.
	gp.stackguard0 = gp.stack.lo + _StackGuard

	msigrestore(gp.m.sigmask)

	gp.m.locks--
}

// Called from syscall package after fork in parent.
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
	systemstack(afterfork)
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
	// It's OK to change the global variable inForkedChild here
	// because we are going to change it back. There is no race here,
	// because if we are sharing address space with the parent process,
	// then the parent process can not be running concurrently.
	inForkedChild = true

	clearSignalHandlers()

	// When we are the child we are the only thread running,
	// so we know that nothing else has changed gp.m.sigmask.
	msigrestore(getg().m.sigmask)

	inForkedChild = false
}

// Called from syscall package before Exec.
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
	// Prevent thread creation during exec.
	execLock.lock()
}

// Called from syscall package after Exec.
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
	execLock.unlock()
}

// todo 内存上初始化一个新的 G
//
// 新分配一个g对象，并申请好 stacksize 大小的栈，此时g的状态为 `_Gidle`
//
// Allocate a new g, with a stack big enough for stacksize bytes.
//
// 分配一个新的g，堆栈的大小足以容纳stacksize个字节   (目前传入 2kb)
func malg(stacksize int32) *g {

	// 创建一个 g 的指针
	newg := new(g)

	// 给 g 分配 内存等操作
	if stacksize >= 0 {
		stacksize = round2(_StackSystem + stacksize)
		systemstack(func() {
			// 调用 stackalloc 分配栈
			newg.stack = stackalloc(uint32(stacksize))
		})
		// 设置 stackguard
		newg.stackguard0 = newg.stack.lo + _StackGuard
		newg.stackguard1 = ^uintptr(0)
		// Clear the bottom word of the stack. We record g
		// there on gsignal stack during VDSO on ARM and ARM64.
		*(*uintptr)(unsafe.Pointer(newg.stack.lo)) = 0
	}
	return newg
}

// Create a new g running fn with siz bytes of arguments.
// Put it on the queue of g's waiting to run.
// The compiler turns a go statement into a call to this.
// Cannot split the stack because it assumes that the arguments
// are available sequentially after &fn; they would not be
// copied if a stack split occurred.
//
//
// 使用 siz个字节 的参数创建一个运行 fn 的 新g
// 并把它放置入 自由g队列中等待唤醒
// 编译器翻译一个 `go func (){} 表达式` 时会调用这个函数
// 无法拆分堆栈，因为它假设参数在 &fn 之后顺序可用; 如果发生堆栈拆分，则不会复制它们。
//
//    todo 新建一个goroutine，
//    用fn + PtrSize 获取第一个参数的地址，也就是argp
//    用siz - 8 获取pc地址
//
// 从goroutine是如何被创建的说起，创建goroutine的函数为：newproc 函数 (在 ./src/runtime/proc.go 文件中)，即：使用go命令创建goroutine时, go会把go命令编译为对runtime.newproc的调用。
//
// 	入参说明:
//
//		siz:   	funcval + 额外参数 的长度
//		fn:		go func() {} 的 func变量   (fn是指向函数机器代码的指针)
//
//go:nosplit
func newproc(siz int32, fn *funcval) {

	// add 是一个指针运算，跳过函数指针    （fn + PtrSize 获取第一个参数的地址）
	// 把栈上的 函数的参数起始地址 找到
	argp := add(unsafe.Pointer(&fn), sys.PtrSize)    // 计算额外参数的地址argp

	// 获取 当前 g,  对于 runtime来说，当前 g 就是 g0  (调用 `getg()` 获取 当前的g, 会编译为读取FS寄存器(TLS), 这里会获取到g0)
	gp := getg()

	// `getcallerpc()` 返回的是 调用函数之后的那条程序指令的地址，
	//  即:  `callee()`函数返回 时要执行的下一条指令的地址.
	// 说白了, 就是程序计数器
	pc := getcallerpc()   // 获取调用端的地址(返回地址)pc

	// 用 g0 的栈创建G对象
	/**
	todo `systemstack()` 会切换 当前的 g 到 g0, 并且使用 g0 的栈空间, 然后调用传入的函数, 再切换回原来的g  和  原来的栈空间
	todo				切换到g0后会 假装返回地址是 mstart, 这样traceback的时候可以在mstart停止。
	todo 				这里传给systemstack的是一个闭包, todo 调用时会把闭包的地址放到寄存器 rdx.

	使用systemstack调用 newproc1()  ===========   用g0的栈创建G对象
	 */
	systemstack(func() {
		// todo 根据  func 地址 和 参数大小 和 计数器 和 g0 等去 创建  语句:`go func () {}` 对应 G实例, 并进入调度
		newproc1(fn, argp, siz, gp, pc)
	})
}

// Create a new g running fn with narg bytes of arguments starting
// at argp. callerpc is the address of the go statement that created
// this. The new g is put on the queue of g's waiting to run.
//
// todo G 的调度
//
// 创建一个新的g，运行fn，其中 narg 个字节的参数从 argp开始。
// callerpc是创建它的go语句的地址。
// 新g放入g等待运行的队列中。
//
// 入参说明:
//		fn: 	`go func (args...) {}` 语句中 函数对应的地址
//		argp:	`go func (args...) {}` 语句中 参数 args 的起始地址
//		narg:	`go func (args...) {}` 语句中 参数 args 的长度
//		callergp: 对 `go func (args...) {}` 语句发起执行动作的 G
// 		callerpc: 程序计数器
//
/**
todo newproc1 --> newg 的大致流程：

newg[gfget] --> nil{is nil?}
nil -->|yes|E[init stack]
nil -->|no|C[malg]
C --> D[set g status=> idle->dead]
D --> allgadd
E --> G[set g status=> dead-> runnable]
allgadd --> G
G --> runqput
 */
func newproc1(fn *funcval, argp unsafe.Pointer, narg int32, callergp *g, callerpc uintptr) {

	// 调用getg获取当前的g, 会编译为读取FS寄存器(TLS), 这里会获取到g0
	_g_ := getg()


	// 如果语句 `go func (args...) {}` 中的 func 为 nil, 则抛出 panic
	if fn == nil {
		_g_.m.throwing = -1 // do not dump full stacks
		throw("go of nil func value")
	}

	// 设置 G 对应的 M 的locks++, 禁止抢占
	// 禁用抢占，因为它可以将 P 保留在本地 var 中
	acquirem() // disable preemption because it can be holding p in a local var
	siz := narg
	siz = (siz + 7) &^ 7

	// We could allocate a larger initial stack if necessary.
	// Not worth it: this is almost always an error.
	// 4*sizeof(uintreg): extra space added below
	// sizeof(uintreg): caller's LR (arm) or return address (x86, in gostartcall).
	//
	// 如果函数的参数大小比 2048 大的话，直接panic
	if siz >= _StackMin-4*sys.RegSize-sys.RegSize {
		throw("newproc: function arguments too large for new goroutine")
	}

	// 从m中获取p
	_p_ := _g_.m.p.ptr()

	// todo 尝试的 获取 P 中的一个 自由G
	newg := gfget(_p_)

	// 如果实在获取不到 自由G,  则 创建一个 新的空闲 G, 并初始化, 并加到 全局 allgs 队列 中
	if newg == nil {

		// todo 内存上初始化一个新的 G
		newg = malg(_StackMin)
		// 初始化 新G 的状态
		//
		// todo 需要先设置g的状态为已中止(_Gdead), 这样gc不会去扫描这个g的未初始化的栈
		casgstatus(newg, _Gidle, _Gdead)

		// 将 新G 追加到 全局的 allgs 队列中      (防止gc扫描清除掉)
		allgadd(newg) // publishes with a g->status of Gdead so GC scanner doesn't look at uninitialized stack.  使用 gdead的g->状态发布，因此GC扫描程序不会查看未初始化的堆栈
	}

	// 做一次 g 的某些状态校验
	if newg.stack.hi == 0 {
		throw("newproc1: newg missing stack")
	}

	// todo 此时获取的 G 一定得是 `_Gdead` 的
	if readgstatus(newg) != _Gdead {
		throw("newproc1: new g is not Gdead")
	}

	// todo 把参数复制到g的栈上
	//
	// 参数大小 + 稍微一点空间
	totalSize := 4*sys.RegSize + uintptr(siz) + sys.MinFrameSize // extra space in case of reads slightly beyond frame    多余的空间，以防读取超出框架
	totalSize += -totalSize & (sys.SpAlign - 1)                  // align to spAlign		对齐以对齐

	// 新协程的栈顶计算，将栈顶减去参数占用的空间
	sp := newg.stack.hi - totalSize
	spArg := sp

	// 把返回地址复制到g的栈上, 这里的返回地址是 goexit(), 表示调用完目标函数后会调用 goexit()
	if usesLR {  // amd64平台下 usesLR 为 false
		// caller's LR
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		prepGoExitFrame(sp)
		spArg += sys.MinFrameSize
	}

	// 如果有参数   (即: go  func (args ...) {} )
	if narg > 0 {

		// copy参数到栈上
		memmove(unsafe.Pointer(spArg), argp, uintptr(narg))
		// This is a stack-to-stack copy. If write barriers
		// are enabled and the source stack is grey (the
		// destination is always black), then perform a
		// barrier copy. We do this *after* the memmove
		// because the destination stack may have garbage on
		// it.
		//
		//
		// 这是一个 堆栈 到 堆栈 的副本.  如果启用了 写入屏障  并且 源堆栈 为灰色 (目标始终为黑色),
		// 则执行屏障复制.  我们在 memmove() 之后执行此操作，因为目标堆栈上可能有垃圾.
		if writeBarrier.needed && !_g_.m.curg.gcscandone {
			f := findfunc(fn.fn)
			stkmap := (*stackmap)(funcdata(f, _FUNCDATA_ArgsPointerMaps))
			if stkmap.nbit > 0 {
				// We're in the prologue, so it's always stack map index 0.
				bv := stackmapdata(stkmap, 0)
				bulkBarrierBitmap(spArg, spArg, uintptr(bv.n)*sys.PtrSize, 0, bv.bytedata)
			}
		}
	}

	// 初始化G的gobuf，保存sp，pc，任务函数等
	//
	// 设置g的 gobuf 数据(sched)
	//
	//			设置sched.sp等于参数+返回地址后的rsp地址
	//			设置sched.pc等于目标函数的地址, 查看gostartcallfn和gostartcall
	//			设置sched.g等于g
	memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))
	// 设置sched.sp等于参数+返回地址后的rsp地址
	newg.sched.sp = sp
	newg.stktopsp = sp

	// 设置 sched.pc 等于目标函数的地址, 查看 gostartcallfn 和 gostartcall
	//
	// todo 保存goexit的地址到sched.pc，后面会调节 goexit 作为任务函数返回后执行的地址，所以goroutine结束后会调用goexit.
	//
	newg.sched.pc = funcPC(goexit) + sys.PCQuantum // +PCQuantum so that previous instruction is in same function  todo  注意这个
	// sched.g 保存当前 新的 G
	newg.sched.g = guintptr(unsafe.Pointer(newg))
	// 将当前的pc压入栈，保存g的任务函数为pc
	gostartcallfn(&newg.sched, fn)
	// gopc 字段保存 newproc() 的pc 值
	newg.gopc = callerpc
	// 设置 debug 期间使用的 祖先g的 pc
	newg.ancestors = saveAncestors(callergp)
	// 任务函数的地址
	newg.startpc = fn.fn
	if _g_.m.curg != nil {
		newg.labels = _g_.m.curg.labels
	}

	// 判断g的任务函数 是不是runtime系统的任务函数，是则 sched.ngsys 做 +1
	if isSystemGoroutine(newg, false) {
		atomic.Xadd(&sched.ngsys, +1)
	}

	// 设置g的状态为  待运行(_Grunnable)
	casgstatus(newg, _Gdead, _Grunnable)

	if _p_.goidcache == _p_.goidcacheend {
		// Sched.goidgen is the last allocated id,
		// this batch must be [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
		// At startup sched.goidgen=0, so main goroutine receives goid=1.
		_p_.goidcache = atomic.Xadd64(&sched.goidgen, _GoidCacheBatch)
		_p_.goidcache -= _GoidCacheBatch - 1
		_p_.goidcacheend = _p_.goidcache + _GoidCacheBatch
	}

	// todo 生成唯一的goid
	newg.goid = int64(_p_.goidcache)
	_p_.goidcache++

	if raceenabled {
		newg.racectx = racegostart(callerpc)
	}
	if trace.enabled {
		// 如果启动了go trace，记录go create事件
		traceGoCreate(newg, newg.startpc)
	}

	// todo 将 g (newg) 放到 p 的 runq 中
	/**
	首先把g放到p.runnext, 如果放到runnext则入队原来在runnext的g
	然后尝试把g放到P的"本地运行队列"
	如果本地运行队列满了则调用runqputslow把g放到"全局运行队列"
					runqputslow会把本地运行队列中一半的g放到全局运行队列, 这样下次就可以继续用快速的本地运行队列了
	 */
	runqput(_p_, newg, true)

	// todo  如果 存在空闲的 P 但是不存在 自旋的M 并且 main函数 已执行  则   唤醒或新建一个M
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 && mainStarted {
		/**
		todo 这一步非常重要, 用于保证当前有足够的M运行G   (唤醒或新建一个M会通过wakep函数)

			首先交换 nmspinning 到1, 成功再继续, 多个线程同时执行wakep只有一个会继续
			调用 `startm()` 函数
					调用 pidleget从"空闲P链表"获取一个空闲的P
					调用mget从"空闲M链表"获取一个空闲的M
					如果没有空闲的M, 则调用newm新建一个M
						newm会新建一个m的实例, m的实例包含一个g0, 然后调用 `newosproc()` 动一个系统线程
						todo newosproc会调用syscall clone创建一个新的线程
						线程创建后会设置TLS, 设置TLS中当前的g为g0, 然后执行 `mstart()`
					调用 notewakeup(&mp.park)唤醒线程
		 */
		wakep() //  唤醒 或 新建一个M   (如果还有空闲P，那么新建M来运行G)
	}


	// todo 调度结束后, 释放被调度的 g 上的 M
	releasem(_g_.m)
}

// saveAncestors copies previous ancestors of the given caller g and
// includes infor for the current caller into a new set of tracebacks for
// a g being created.
func saveAncestors(callergp *g) *[]ancestorInfo {
	// Copy all prior info, except for the root goroutine (goid 0).
	if debug.tracebackancestors <= 0 || callergp.goid == 0 {
		return nil
	}
	var callerAncestors []ancestorInfo
	if callergp.ancestors != nil {
		callerAncestors = *callergp.ancestors
	}
	n := int32(len(callerAncestors)) + 1
	if n > debug.tracebackancestors {
		n = debug.tracebackancestors
	}
	ancestors := make([]ancestorInfo, n)
	copy(ancestors[1:], callerAncestors)

	var pcs [_TracebackMaxFrames]uintptr
	npcs := gcallers(callergp, 0, pcs[:])
	ipcs := make([]uintptr, npcs)
	copy(ipcs, pcs[:])
	ancestors[0] = ancestorInfo{
		pcs:  ipcs,
		goid: callergp.goid,
		gopc: callergp.gopc,
	}

	ancestorsp := new([]ancestorInfo)
	*ancestorsp = ancestors
	return ancestorsp
}

// 将 自由G 放入 P 的 gFree
//
// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
func gfput(_p_ *p, gp *g) {
	if readgstatus(gp) != _Gdead {
		throw("gfput: bad status (not Gdead)")
	}

	stksize := gp.stack.hi - gp.stack.lo

	if stksize != _FixedStack {
		// non-standard stack size - free it.
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		gp.stackguard0 = 0
	}

	_p_.gFree.push(gp)
	_p_.gFree.n++
	if _p_.gFree.n >= 64 {
		lock(&sched.gFree.lock)
		for _p_.gFree.n >= 32 {
			_p_.gFree.n--
			gp = _p_.gFree.pop()
			if gp.stack.lo == 0 {
				sched.gFree.noStack.push(gp)
			} else {
				sched.gFree.stack.push(gp)
			}
			sched.gFree.n++
		}
		unlock(&sched.gFree.lock)
	}
}

// todo 获取 P 中的一个 自由G
//
// Get from gfree list.
// If local list is empty, grab a batch from global list.
//
// 	从 P 的 gFree 队列中 中获取 一个 自由G
// 	如果 gFree列表为空，从 调度器 (schedt) 的 gFree 中抓取一批  自由G 放到 当前 P的 gFree 队列中,
// 并 弹出 队首的 自由G
func gfget(_p_ *p) *g {
retry:
	if _p_.gFree.empty() && (!sched.gFree.stack.empty() || !sched.gFree.noStack.empty()) {
		lock(&sched.gFree.lock)
		// Move a batch of free Gs to the P.
		for _p_.gFree.n < 32 {
			// Prefer Gs with stacks.
			gp := sched.gFree.stack.pop()
			if gp == nil {
				gp = sched.gFree.noStack.pop()
				if gp == nil {
					break
				}
			}
			sched.gFree.n--
			_p_.gFree.push(gp)
			_p_.gFree.n++
		}
		unlock(&sched.gFree.lock)
		goto retry
	}
	gp := _p_.gFree.pop()
	if gp == nil {
		return nil
	}
	_p_.gFree.n--
	if gp.stack.lo == 0 {
		// Stack was deallocated in gfput. Allocate a new one.
		systemstack(func() {
			gp.stack = stackalloc(_FixedStack)
		})
		gp.stackguard0 = gp.stack.lo + _StackGuard
	} else {
		if raceenabled {
			racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if msanenabled {
			msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
	}
	return gp
}

// Purge all cached G's from gfree list to the global list.
func gfpurge(_p_ *p) {
	lock(&sched.gFree.lock)
	for !_p_.gFree.empty() {
		gp := _p_.gFree.pop()
		_p_.gFree.n--
		if gp.stack.lo == 0 {
			sched.gFree.noStack.push(gp)
		} else {
			sched.gFree.stack.push(gp)
		}
		sched.gFree.n++
	}
	unlock(&sched.gFree.lock)
}

// Breakpoint executes a breakpoint trap.    `Breakpoint()` 执行断点陷阱
func Breakpoint() {
	breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//go:nosplit
func dolockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	_g_ := getg()
	_g_.m.lockedg.set(_g_)
	_g_.lockedm.set(_g_.m)
}

//go:nosplit

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// UnlockOSThread as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// All init functions are run on the startup thread. Calling LockOSThread
// from an init function will cause the main function to be invoked on
// that thread.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
func LockOSThread() {
	if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
		// If we need to start a new thread from the locked
		// thread, we need the template thread. Start it now
		// while we're in a known-good state.
		startTemplateThread()
	}
	_g_ := getg()
	_g_.m.lockedExt++
	if _g_.m.lockedExt == 0 {
		_g_.m.lockedExt--
		panic("LockOSThread nesting overflow")
	}
	dolockOSThread()
}

//go:nosplit
func lockOSThread() {
	getg().m.lockedInt++
	dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
//go:nosplit
func dounlockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	_g_ := getg()
	if _g_.m.lockedInt != 0 || _g_.m.lockedExt != 0 {
		return
	}
	_g_.m.lockedg = 0
	_g_.lockedm = 0
}

//go:nosplit

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
func UnlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedExt == 0 {
		return
	}
	_g_.m.lockedExt--
	dounlockOSThread()
}

//go:nosplit
func unlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedInt == 0 {
		systemstack(badunlockosthread)
	}
	_g_.m.lockedInt--
	dounlockOSThread()
}

func badunlockosthread() {
	throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

func gcount() int32 {
	n := int32(allglen) - sched.gFree.n - int32(atomic.Load(&sched.ngsys))
	for _, _p_ := range allp {
		n -= _p_.gFree.n
	}

	// All these variables can be changed concurrently, so the result can be inconsistent.
	// But at least the current goroutine is running.
	if n < 1 {
		n = 1
	}
	return n
}

func mcount() int32 {
	return int32(sched.mnext - sched.nmfreed)
}

var prof struct {
	signalLock uint32
	hz         int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }
func _VDSO()                      { _VDSO() }

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
	if prof.hz == 0 {
		return
	}

	// On mips{,le}, 64bit atomics are emulated with spinlocks, in
	// runtime/internal/atomic. If SIGPROF arrives while the program is inside
	// the critical section, it creates a deadlock (when writing the sample).
	// As a workaround, create a counter of SIGPROFs while in critical section
	// to store the count, and pass it to sigprof.add() later when SIGPROF is
	// received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
	if GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm" {
		if f := findfunc(pc); f.valid() {
			if hasPrefix(funcname(f), "runtime/internal/atomic") {
				cpuprof.lostAtomic++
				return
			}
		}
	}

	// Profiling runs concurrently with GC, so it must not allocate.
	// Set a trap in case the code does allocate.
	// Note that on windows, one thread takes profiles of all the
	// other threads, so mp is usually not getg().m.
	// In fact mp may not even be stopped.
	// See golang.org/issue/17165.
	getg().m.mallocing++

	// Define that a "user g" is a user-created goroutine, and a "system g"
	// is one that is m->g0 or m->gsignal.
	//
	// We might be interrupted for profiling halfway through a
	// goroutine switch. The switch involves updating three (or four) values:
	// g, PC, SP, and (on arm) LR. The PC must be the last to be updated,
	// because once it gets updated the new g is running.
	//
	// When switching from a user g to a system g, LR is not considered live,
	// so the update only affects g, SP, and PC. Since PC must be last, there
	// the possible partial transitions in ordinary execution are (1) g alone is updated,
	// (2) both g and SP are updated, and (3) SP alone is updated.
	// If SP or g alone is updated, we can detect the partial transition by checking
	// whether the SP is within g's stack bounds. (We could also require that SP
	// be changed only after g, but the stack bounds check is needed by other
	// cases, so there is no need to impose an additional requirement.)
	//
	// There is one exceptional transition to a system g, not in ordinary execution.
	// When a signal arrives, the operating system starts the signal handler running
	// with an updated PC and SP. The g is updated last, at the beginning of the
	// handler. There are two reasons this is okay. First, until g is updated the
	// g and SP do not match, so the stack bounds check detects the partial transition.
	// Second, signal handlers currently run with signals disabled, so a profiling
	// signal cannot arrive during the handler.
	//
	// When switching from a system g to a user g, there are three possibilities.
	//
	// First, it may be that the g switch has no PC update, because the SP
	// either corresponds to a user g throughout (as in asmcgocall)
	// or because it has been arranged to look like a user g frame
	// (as in cgocallback_gofunc). In this case, since the entire
	// transition is a g+SP update, a partial transition updating just one of
	// those will be detected by the stack bounds check.
	//
	// Second, when returning from a signal handler, the PC and SP updates
	// are performed by the operating system in an atomic update, so the g
	// update must be done before them. The stack bounds check detects
	// the partial transition here, and (again) signal handlers run with signals
	// disabled, so a profiling signal cannot arrive then anyway.
	//
	// Third, the common case: it may be that the switch updates g, SP, and PC
	// separately. If the PC is within any of the functions that does this,
	// we don't ask for a traceback. C.F. the function setsSP for more about this.
	//
	// There is another apparently viable approach, recorded here in case
	// the "PC within setsSP function" check turns out not to be usable.
	// It would be possible to delay the update of either g or SP until immediately
	// before the PC update instruction. Then, because of the stack bounds check,
	// the only problematic interrupt point is just before that PC update instruction,
	// and the sigprof handler can detect that instruction and simulate stepping past
	// it in order to reach a consistent state. On ARM, the update of g must be made
	// in two places (in R10 and also in a TLS slot), so the delayed update would
	// need to be the SP update. The sigprof handler must read the instruction at
	// the current PC and if it was the known instruction (for example, JMP BX or
	// MOV R2, PC), use that other register in place of the PC value.
	// The biggest drawback to this solution is that it requires that we can tell
	// whether it's safe to read from the memory pointed at by PC.
	// In a correct program, we can test PC == nil and otherwise read,
	// but if a profiling signal happens at the instant that a program executes
	// a bad jump (before the program manages to handle the resulting fault)
	// the profiling handler could fault trying to read nonexistent memory.
	//
	// To recap, there are no constraints on the assembly being used for the
	// transition. We simply require that g and SP match and that the PC is not
	// in gogo.
	traceback := true
	if gp == nil || sp < gp.stack.lo || gp.stack.hi < sp || setsSP(pc) || (mp != nil && mp.vdsoSP != 0) {
		traceback = false
	}
	var stk [maxCPUProfStack]uintptr
	n := 0
	if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
		cgoOff := 0
		// Check cgoCallersUse to make sure that we are not
		// interrupting other code that is fiddling with
		// cgoCallers.  We are running in a signal handler
		// with all signals blocked, so we don't have to worry
		// about any other code interrupting us.
		if atomic.Load(&mp.cgoCallersUse) == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
			for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
				cgoOff++
			}
			copy(stk[:], mp.cgoCallers[:cgoOff])
			mp.cgoCallers[0] = 0
		}

		// Collect Go stack that leads to the cgo call.
		n = gentraceback(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, 0, &stk[cgoOff], len(stk)-cgoOff, nil, nil, 0)
		if n > 0 {
			n += cgoOff
		}
	} else if traceback {
		n = gentraceback(pc, sp, lr, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
	}

	if n <= 0 {
		// Normal traceback is impossible or has failed.
		// See if it falls into several common cases.
		n = 0
		if (GOOS == "windows" || GOOS == "solaris" || GOOS == "illumos" || GOOS == "darwin" || GOOS == "aix") && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
			// Libcall, i.e. runtime syscall on windows.
			// Collect Go stack that leads to the call.
			n = gentraceback(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), 0, &stk[0], len(stk), nil, nil, 0)
		}
		if n == 0 && mp != nil && mp.vdsoSP != 0 {
			n = gentraceback(mp.vdsoPC, mp.vdsoSP, 0, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
		}
		if n == 0 {
			// If all of the above has failed, account it against abstract "System" or "GC".
			n = 2
			if inVDSOPage(pc) {
				pc = funcPC(_VDSO) + sys.PCQuantum
			} else if pc > firstmoduledata.etext {
				// "ExternalCode" is better than "etext".
				pc = funcPC(_ExternalCode) + sys.PCQuantum
			}
			stk[0] = pc
			if mp.preemptoff != "" {
				stk[1] = funcPC(_GC) + sys.PCQuantum
			} else {
				stk[1] = funcPC(_System) + sys.PCQuantum
			}
		}
	}

	if prof.hz != 0 {
		cpuprof.add(gp, stk[:n])
	}
	getg().m.mallocing--
}

// If the signal handler receives a SIGPROF signal on a non-Go thread,
// it tries to collect a traceback into sigprofCallers.
// sigprofCallersUse is set to non-zero while sigprofCallers holds a traceback.
var sigprofCallers cgoCallers
var sigprofCallersUse uint32

// sigprofNonGo is called if we receive a SIGPROF signal on a non-Go thread,
// and the signal handler collected a stack trace in sigprofCallers.
// When this is called, sigprofCallersUse will be non-zero.
// g is nil, and what we can do is very limited.
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGo() {
	if prof.hz != 0 {
		n := 0
		for n < len(sigprofCallers) && sigprofCallers[n] != 0 {
			n++
		}
		cpuprof.addNonGo(sigprofCallers[:n])
	}

	atomic.Store(&sigprofCallersUse, 0)
}

// sigprofNonGoPC is called when a profiling signal arrived on a
// non-Go thread and we have a single PC value, not a stack trace.
// g is nil, and what we can do is very limited.
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGoPC(pc uintptr) {
	if prof.hz != 0 {
		stk := []uintptr{
			pc,
			funcPC(_ExternalCode) + sys.PCQuantum,
		}
		cpuprof.addNonGo(stk)
	}
}

// Reports whether a function will set the SP
// to an absolute value. Important that
// we don't traceback when these are at the bottom
// of the stack since we can't be sure that we will
// find the caller.
//
// If the function is not on the bottom of the stack
// we assume that it will have set it up so that traceback will be consistent,
// either by being a traceback terminating function
// or putting one on the stack at the right offset.
func setsSP(pc uintptr) bool {
	f := findfunc(pc)
	if !f.valid() {
		// couldn't find the function for this PC,
		// so assume the worst and stop traceback
		return true
	}
	switch f.funcID {
	case funcID_gogo, funcID_systemstack, funcID_mcall, funcID_morestack:
		return true
	}
	return false
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
func setcpuprofilerate(hz int32) {
	// Force sane arguments.
	if hz < 0 {
		hz = 0
	}

	// Disable preemption, otherwise we can be rescheduled to another thread
	// that has profiling enabled.
	_g_ := getg()
	_g_.m.locks++

	// Stop profiler on this thread so that it is safe to lock prof.
	// if a profiling signal came in while we had prof locked,
	// it would deadlock.
	setThreadCPUProfiler(0)

	for !atomic.Cas(&prof.signalLock, 0, 1) {
		osyield()
	}
	if prof.hz != hz {
		setProcessCPUProfiler(hz)
		prof.hz = hz
	}
	atomic.Store(&prof.signalLock, 0)

	lock(&sched.lock)
	sched.profilehz = hz
	unlock(&sched.lock)

	if hz != 0 {
		setThreadCPUProfiler(hz)
	}

	_g_.m.locks--
}

// init initializes pp, which may be a freshly allocated p or a
// previously destroyed p, and transitions it to status _Pgcstop.
func (pp *p) init(id int32) {  // 初始化P
	pp.id = id
	pp.status = _Pgcstop
	pp.sudogcache = pp.sudogbuf[:0]
	for i := range pp.deferpool {
		pp.deferpool[i] = pp.deferpoolbuf[i][:0]
	}
	pp.wbBuf.reset()
	if pp.mcache == nil {
		if id == 0 {
			if getg().m.mcache == nil {
				throw("missing mcache?")
			}
			pp.mcache = getg().m.mcache // bootstrap
		} else {
			pp.mcache = allocmcache()
		}
	}
	if raceenabled && pp.raceprocctx == 0 {
		if id == 0 {
			pp.raceprocctx = raceprocctx0
			raceprocctx0 = 0 // bootstrap
		} else {
			pp.raceprocctx = raceproccreate()
		}
	}
}
// todo 释放P的资源   销毁P   (状态： Pdead)
//
// destroy releases all of the resources associated with pp and
// transitions it to status _Pdead.
//
// sched.lock must be held and the world must be stopped.
func (pp *p) destroy() {
	// Move all runnable goroutines to the global queue
	for pp.runqhead != pp.runqtail {
		// Pop from tail of local queue
		pp.runqtail--
		gp := pp.runq[pp.runqtail%uint32(len(pp.runq))].ptr()
		// Push onto head of global queue
		globrunqputhead(gp)
	}
	if pp.runnext != 0 {
		globrunqputhead(pp.runnext.ptr())
		pp.runnext = 0
	}
	if len(pp.timers) > 0 {
		plocal := getg().m.p.ptr()  // 获取 当前G 的P
		// The world is stopped, but we acquire timersLock to
		// protect against sysmon calling timeSleepUntil.
		// This is the only case where we hold the timersLock of
		// more than one P, so there are no deadlock concerns.
		lock(&plocal.timersLock)
		lock(&pp.timersLock)
		moveTimers(plocal, pp.timers)  // 将 pp 中的 定时器 转移到  plocal  这个p上
		pp.timers = nil
		pp.numTimers = 0
		pp.adjustTimers = 0
		pp.deletedTimers = 0
		atomic.Store64(&pp.timer0When, 0)
		unlock(&pp.timersLock)
		unlock(&plocal.timersLock)
	}
	// If there's a background worker, make it runnable and put
	// it on the global queue so it can clean itself up.
	if gp := pp.gcBgMarkWorker.ptr(); gp != nil {
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.enabled {
			traceGoUnpark(gp, 0)
		}
		globrunqput(gp)
		// This assignment doesn't race because the
		// world is stopped.
		pp.gcBgMarkWorker.set(nil)
	}
	// Flush p's write barrier buffer.
	if gcphase != _GCoff {
		wbBufFlush1(pp)
		pp.gcw.dispose()
	}
	for i := range pp.sudogbuf {
		pp.sudogbuf[i] = nil
	}
	pp.sudogcache = pp.sudogbuf[:0]
	for i := range pp.deferpool {
		for j := range pp.deferpoolbuf[i] {
			pp.deferpoolbuf[i][j] = nil
		}
		pp.deferpool[i] = pp.deferpoolbuf[i][:0]
	}
	systemstack(func() {
		for i := 0; i < pp.mspancache.len; i++ {
			// Safe to call since the world is stopped.
			mheap_.spanalloc.free(unsafe.Pointer(pp.mspancache.buf[i]))
		}
		pp.mspancache.len = 0
		pp.pcache.flush(&mheap_.pages)
	})
	freemcache(pp.mcache)
	pp.mcache = nil
	gfpurge(pp)
	traceProcFree(pp)
	if raceenabled {
		if pp.timerRaceCtx != 0 {
			// The race detector code uses a callback to fetch
			// the proc context, so arrange for that callback
			// to see the right thing.
			// This hack only works because we are the only
			// thread running.
			mp := getg().m
			phold := mp.p.ptr()
			mp.p.set(pp)

			racectxend(pp.timerRaceCtx)
			pp.timerRaceCtx = 0

			mp.p.set(phold)
		}
		raceprocdestroy(pp.raceprocctx)
		pp.raceprocctx = 0
	}
	pp.gcAssistTime = 0
	pp.status = _Pdead  // _Pdead 状态的 P 在任何流程都不会被处理, 最终被 GC 掉 todo (做法是 比较 p.status 从来不用 _Pdead )
}

// Change number of processors. The world is stopped, sched is locked.
// gcworkbufs are not being modified by either the GC or
// the write barrier code.
// Returns list of Ps with local work, they need to be scheduled by the caller.
func procresize(nprocs int32) *p {
	old := gomaxprocs
	if old < 0 || nprocs <= 0 {
		throw("procresize: invalid arg")
	}
	if trace.enabled {
		traceGomaxprocs(nprocs)
	}

	// update statistics
	now := nanotime()
	if sched.procresizetime != 0 {
		sched.totaltime += int64(old) * (now - sched.procresizetime)
	}
	sched.procresizetime = now

	// Grow allp if necessary.
	if nprocs > int32(len(allp)) {
		// Synchronize with retake, which could be running
		// concurrently since it doesn't run on a P.
		lock(&allpLock)
		if nprocs <= int32(cap(allp)) {
			allp = allp[:nprocs]
		} else {
			nallp := make([]*p, nprocs)
			// Copy everything up to allp's cap so we
			// never lose old allocated Ps.
			copy(nallp, allp[:cap(allp)])
			allp = nallp
		}
		unlock(&allpLock)
	}

	// initialize new P's
	for i := old; i < nprocs; i++ {
		pp := allp[i]
		if pp == nil {
			pp = new(p) // 创建P
		}
		pp.init(i)
		atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
	}

	_g_ := getg()
	if _g_.m.p != 0 && _g_.m.p.ptr().id < nprocs {
		// continue to use the current P
		_g_.m.p.ptr().status = _Prunning
		_g_.m.p.ptr().mcache.prepareForSweep()
	} else {
		// release the current P and acquire allp[0].
		//
		// We must do this before destroying our current P
		// because p.destroy itself has write barriers, so we
		// need to do that from a valid P.
		if _g_.m.p != 0 {
			if trace.enabled {
				// Pretend that we were descheduled
				// and then scheduled again to keep
				// the trace sane.
				traceGoSched()
				traceProcStop(_g_.m.p.ptr())
			}
			_g_.m.p.ptr().m = 0
		}
		_g_.m.p = 0
		_g_.m.mcache = nil
		p := allp[0]
		p.m = 0
		p.status = _Pidle
		acquirep(p)
		if trace.enabled {
			traceGoStart()
		}
	}

	// release resources from unused P's
	for i := nprocs; i < old; i++ {
		p := allp[i]
		p.destroy()
		// can't free P itself because it can be referenced by an M in syscall
	}

	// Trim allp.
	if int32(len(allp)) != nprocs {
		lock(&allpLock)
		allp = allp[:nprocs]
		unlock(&allpLock)
	}

	var runnablePs *p
	for i := nprocs - 1; i >= 0; i-- {
		p := allp[i]
		if _g_.m.p.ptr() == p {
			continue
		}
		p.status = _Pidle
		if runqempty(p) {
			pidleput(p)
		} else {
			p.m.set(mget())
			p.link.set(runnablePs)
			runnablePs = p
		}
	}
	stealOrder.reset(uint32(nprocs))
	var int32p *int32 = &gomaxprocs // make compiler check that gomaxprocs is an int32
	atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
	return runnablePs
}

// Associate p and the current m.
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires _p_.
//
//
// 关联 P 和 当前 M
//
//  即使不是调用方，此函数也具有 写障碍，因为它立即获取 _p_
//
//
//go:yeswritebarrierrec
func acquirep(_p_ *p) {
	// Do the part that isn't allowed to have write barriers.   做不允许 写屏障 的部分
	wirep(_p_)

	// Have p; write barriers now allowed.

	// Perform deferred mcache flush before this P can allocate
	// from a potentially stale mcache.
	_p_.mcache.prepareForSweep()

	if trace.enabled {
		traceProcStart()
	}
}

// wirep is the first step of acquirep, which actually associates the
// current M to _p_. This is broken out so we can disallow write
// barriers for this part, since we don't yet have a P.
//
//go:nowritebarrierrec
//go:nosplit
func wirep(_p_ *p) {
	_g_ := getg()

	if _g_.m.p != 0 || _g_.m.mcache != nil {
		throw("wirep: already in go")
	}
	if _p_.m != 0 || _p_.status != _Pidle {
		id := int64(0)
		if _p_.m != 0 {
			id = _p_.m.ptr().id
		}
		print("wirep: p->m=", _p_.m, "(", id, ") p->status=", _p_.status, "\n")
		throw("wirep: invalid p state")
	}
	_g_.m.mcache = _p_.mcache
	_g_.m.p.set(_p_)
	_p_.m.set(_g_.m)
	_p_.status = _Prunning
}

// 释放 M 拥有的 P, P会变为空闲(_Pidle)状态
//
// Disassociate p and the current m.
func releasep() *p {
	_g_ := getg()

	if _g_.m.p == 0 || _g_.m.mcache == nil {
		throw("releasep: invalid arg")
	}
	_p_ := _g_.m.p.ptr()
	if _p_.m.ptr() != _g_.m || _p_.mcache != _g_.m.mcache || _p_.status != _Prunning {
		print("releasep: m=", _g_.m, " m->p=", _g_.m.p.ptr(), " p->m=", hex(_p_.m), " m->mcache=", _g_.m.mcache, " p->mcache=", _p_.mcache, " p->status=", _p_.status, "\n")
		throw("releasep: invalid p state")
	}
	if trace.enabled {
		traceProcStop(_g_.m.p.ptr())
	}
	_g_.m.p = 0
	_g_.m.mcache = nil
	_p_.m = 0
	_p_.status = _Pidle
	return _p_
}

func incidlelocked(v int32) {
	lock(&sched.lock)
	sched.nmidlelocked += v
	if v > 0 {
		checkdead()
	}
	unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
//
//
//  检查死锁情况
//
//		检查是 基于 正在运行的 M 的数量, 如果为 0 则需要报 "deadlock"
//
//		必须加锁操作
//
func checkdead() {
	// For -buildmode=c-shared or -buildmode=c-archive it's OK if
	// there are no running goroutines. The calling program is
	// assumed to be running.
	if islibrary || isarchive {
		return
	}

	// If we are dying because of a signal caught on an already idle thread,
	// freezetheworld will cause all running threads to block.
	// And runtime will essentially enter into deadlock state,
	// except that there is a thread that will call exit soon.
	if panicking > 0 {
		return
	}

	// If we are not running under cgo, but we have an extra M then account
	// for it. (It is possible to have an extra M on Windows without cgo to
	// accommodate callbacks created by syscall.NewCallback. See issue #6751
	// for details.)
	var run0 int32
	if !iscgo && cgoHasExtraM {
		mp := lockextra(true)
		haveExtraM := extraMCount > 0
		unlockextra(mp)
		if haveExtraM {
			run0 = 1
		}
	}

	run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
	if run > run0 {
		return
	}
	if run < 0 {
		print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
		throw("checkdead: inconsistent counts")
	}

	grunning := 0
	lock(&allglock)
	for i := 0; i < len(allgs); i++ {
		gp := allgs[i]
		if isSystemGoroutine(gp, false) {
			continue
		}
		s := readgstatus(gp)
		switch s &^ _Gscan {
		case _Gwaiting,
			_Gpreempted:
			grunning++
		case _Grunnable,
			_Grunning,
			_Gsyscall:
			unlock(&allglock)
			print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
			throw("checkdead: runnable g")
		}
	}
	unlock(&allglock)
	if grunning == 0 { // possible if main goroutine calls runtime·Goexit()
		unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
		throw("no goroutines (main called runtime.Goexit) - deadlock!")
	}

	// Maybe jump time forward for playground.
	if faketime != 0 {
		when, _p_ := timeSleepUntil()
		if _p_ != nil {
			faketime = when
			for pp := &sched.pidle; *pp != 0; pp = &(*pp).ptr().link {
				if (*pp).ptr() == _p_ {
					*pp = _p_.link
					break
				}
			}
			mp := mget()
			if mp == nil {
				// There should always be a free M since
				// nothing is running.
				throw("checkdead: no m for timer")
			}
			mp.nextp.set(_p_)
			notewakeup(&mp.park)
			return
		}
	}

	// There are no goroutines running, so we can look at the P's.
	for _, _p_ := range allp {
		if len(_p_.timers) > 0 {
			return
		}
	}

	getg().m.throwing = -1 // do not dump full stacks
	unlock(&sched.lock)    // unlock so that GODEBUG=scheddetail=1 doesn't hang
	throw("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
//
// forcegcperiod 是垃圾回收之间的最长时间（以纳秒为单位）.
// 				如果我们在没有垃圾收集的情况下走了这么长时间，就会被迫运行。
//
//	这是用于测试的变量. 它通常不会改变.
//
// todo forcegcperiod 的定义是 2 分钟, 也就是2分钟内没有执行过GC就会强制触发
//
var forcegcperiod int64 = 2 * 60 * 1e9     // todo 定时 gc 的间隔  2分钟



// todo 运行sysmon系统后台监控.   抢占函数  【超级重要】  =================   该函数 只有在 runtime.main() 中被调用
//
// 				todo		sysmon() 会监控整个调度系统，如果 某个G 长时间占用cpu，会被标记为可抢占
//
// runtime.main() 会创建一个额外的M  运行sysmon函数, 抢占就是在sysmon中实现的
//
// 				 我们知道 M 需要绑定 P 才能执行 G 即可， todo 这个规则有一个例外，就是 sysmon ， 它不需要P，它是直接运行在M上，相当于【G M 模型】.  sysmon 更像C语言的线程上的  任务函数.
//
// Always runs without a P, so write barriers are not allowed.
//
//go:nowritebarrierrec
func sysmon() {

	// sysmon() 中有 netpool(获取fd事件), retake(抢占), forcegc(按时间强制执行gc), scavenge heap(释放自由列表中多余的项减少内存占用)  等处理

	// cgo 和 syscall 时，p的状态会被设置为 `_Psyscall`， sysmon() 周期性地检查并 retake p，
	// 如果发现 P 处于这个状态且超过10ms就会强制性收回 P，M 从 cgo 和 syscall 返回后会重新尝试拿 P，进入调度循环

	lock(&sched.lock)
	sched.nmsys++

	// 后台监控 会先检查程序是否死锁，很多初学者跑一个go程序会报 "all goroutines are asleep - deadlock!" ，就是在这里报的
	checkdead()
	unlock(&sched.lock)

	lasttrace := int64(0)
	idle := 0 // how many cycles in succession we had not wokeup somebody
	delay := uint32(0)



	// todo sysmon 会进入一个无限循环, 第一轮回休眠20us, 之后每次休眠时间倍增, 最终每一轮都会休眠10ms.
	for {
		if idle == 0 { // start with 20us sleep...
			delay = 20
		} else if idle > 50 { // start doubling the sleep after 1ms...
			delay *= 2
		}
		if delay > 10*1000 { // up to 10ms
			delay = 10 * 1000
		}
		// 休眠delay us
		usleep(delay)
		now := nanotime()
		next, _ := timeSleepUntil()
		if debug.schedtrace <= 0 && (sched.gcwaiting != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs)) {
			lock(&sched.lock)
			if atomic.Load(&sched.gcwaiting) != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs) {
				if next > now {
					atomic.Store(&sched.sysmonwait, 1)
					unlock(&sched.lock)
					// Make wake-up period small enough
					// for the sampling to be correct.
					sleep := forcegcperiod / 2
					if next-now < sleep {
						sleep = next - now
					}
					shouldRelax := sleep >= osRelaxMinNS
					if shouldRelax {
						osRelax(true)
					}
					notetsleep(&sched.sysmonnote, sleep)
					if shouldRelax {
						osRelax(false)
					}
					now = nanotime()
					next, _ = timeSleepUntil()
					lock(&sched.lock)
					atomic.Store(&sched.sysmonwait, 0)
					noteclear(&sched.sysmonnote)
				}
				idle = 0
				delay = 20
			}
			unlock(&sched.lock)
		}
		// trigger libc interceptors if needed
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		// poll network if not polled for more than 10ms
		lastpoll := int64(atomic.Load64(&sched.lastpoll))

		// netpoll 网络轮询器 相关
		//
		// 如果超过10ms都没进行 netpoll ，那么强制执行一次 netpoll，
		// 并且如果获取到了可运行的G，那么插入全局列表.
		if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
			atomic.Cas64(&sched.lastpoll, uint64(lastpoll), uint64(now))
			list := netpoll(0) // non-blocking - returns list of goroutines
			if !list.empty() {
				// Need to decrement number of idle locked M's
				// (pretending that one more is running) before injectglist.
				// Otherwise it can lead to the following situation:
				// injectglist grabs all P's but before it starts M's to run the P's,
				// another M returns from syscall, finishes running its G,
				// observes that there is no work to do and no other running M's
				// and reports deadlock.
				incidlelocked(-1)
				injectglist(&list)
				incidlelocked(1)
			}
		}
		if next < now {
			// There are timers that should have already run,
			// perhaps because there is an unpreemptible P.
			// Try to start an M to run them.
			startm(nil, false)
		}
		// retake P's blocked in syscalls
		// and preempt long running G's
		//
		// 抢夺阻塞时间长的 syscall 的G
		if retake(now) != 0 {
			idle = 0
		} else {
			idle++
		}
		// check if we need to force a GC
		if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && atomic.Load(&forcegc.idle) != 0 {
			lock(&forcegc.lock)
			forcegc.idle = 0
			var list gList
			list.push(forcegc.g)
			injectglist(&list)
			unlock(&forcegc.lock)
		}
		if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
			lasttrace = now
			schedtrace(debug.scheddetail > 0)
		}
	}
}

type sysmontick struct {
	schedtick   uint32
	schedwhen   int64
	syscalltick uint32
	syscallwhen int64
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

/**
retake() 函数 会遍历所有的 P，如果一个P处于执行状态，且已经连续执行了较长时间，就会被抢占.

TODO 抢占的情况分为两种：

		1、 P处于系统调用中:   	当P处于系统调用状态超过10ms，那么调用 handoffp 来检查该P下是否有其他可运行的G。
								如果有的话， 调用 startm() 来常获取一个M 或 新建一个M 来服务.
								todo 【这就是为啥cgo里阻塞很久 或者 系统调用阻塞很久， 会导致runtime创建很多线程的原因】.
								这里还有一个要注意的地方就是在设置 p.status之前，P已经丢弃了M.

		2、P处于正在运行中： 	检查 G 运行时间超过 `forcePreemptNS(10ms)`，则抢占这个P，todo 当然这个抢占不是主动的，而是被动的，
								通过 preemptone 设置P的 stackguard0 设为 `stackPreempt` 和 gp.preempt = true ，导致该P中正在执行的G进行下一次函数调用时， 导致栈空间检查失败.
								进而触发 morestack（汇编代码，位于asm_XXX.s中），然后进行一连串的函数调用，
								最终会调用 goschedImpl() 函数，进行解除P与当前M的关联，让该G进入 `_Grunnable` 状态，插入 schedt 的全局G列表，等待下次调度.
								触发的一系列函数如下：
										todo     morestack() -> newstack() -> gopreempt_m() -> goschedImpl() -> schedule()

TODO 自此抢占原理已明了:

		第一种情况:   	是为了防止系统调用的G，导致P里面的其他G无法调度，也就是饿死的状态.
		第二种情况:		是为何防止一个G运行太长时间，而导致P里面的其他G饿死的状态.
 */
func retake(now int64) uint32 {
	n := 0
	// Prevent allp slice changes. This lock will be completely
	// uncontended unless we're already stopping the world.
	lock(&allpLock)
	// We can't use a range loop over allp because we may
	// temporarily drop the allpLock. Hence, we need to re-fetch
	// allp each time around the loop.
	//
	// 枚举所有的P
	for i := 0; i < len(allp); i++ {
		_p_ := allp[i]
		if _p_ == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			continue
		}
		pd := &_p_.sysmontick
		s := _p_.status
		sysretake := false

		// 如果P在运行中(_Prunning), 且经过了一次sysmon循环并且G运行时间超过forcePreemptNS(10ms), 则抢占这个P
		if s == _Prunning || s == _Psyscall {
			// Preempt G if it's running for too long.
			t := int64(_p_.schedtick)
			if int64(pd.schedtick) != t {
				pd.schedtick = uint32(t)
				pd.schedwhen = now
			} else if pd.schedwhen+forcePreemptNS <= now {
				preemptone(_p_)
				// In case of syscall, preemptone() doesn't
				// work, because there is no M wired to P.
				sysretake = true
			}
		}

		// 如果P在系统调用中(_Psyscall), 且经过了一次sysmon循环(20us~10ms), 则抢占这个P
		if s == _Psyscall {
			// Retake P from syscall if it's there for more than 1 sysmon tick (at least 20us).
			t := int64(_p_.syscalltick)
			if !sysretake && int64(pd.syscalltick) != t {
				pd.syscalltick = uint32(t)
				pd.syscallwhen = now
				continue
			}
			// On the one hand we don't want to retake Ps if there is no other work to do,
			// but on the other hand we want to retake them eventually
			// because they can prevent the sysmon thread from deep sleep.
			if runqempty(_p_) && atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) > 0 && pd.syscallwhen+10*1000*1000 > now {
				continue
			}
			// Drop allpLock so we can take sched.lock.
			unlock(&allpLock)
			// Need to decrement number of idle locked M's
			// (pretending that one more is running) before the CAS.
			// Otherwise the M from which we retake can exit the syscall,
			// increment nmidle and report deadlock.
			incidlelocked(-1)
			if atomic.Cas(&_p_.status, s, _Pidle) {
				if trace.enabled {
					traceGoSysBlock(_p_)
					traceProcStop(_p_)
				}
				n++
				_p_.syscalltick++
				handoffp(_p_)  // 解除 M 和 P 之间的关联
			}
			incidlelocked(1)
			lock(&allpLock)
		}
	}
	unlock(&allpLock)
	return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
//
// todo 尝试向 所有 G 发起 (被强占 要求停止的通知)
//
//  告诉所有goroutine，它们已经被抢占并且应该停止。
//  此功能纯粹是尽力而为。 如果处理器刚刚开始运行goroutine，它可能无法通知该goroutine。
//  无需持有任何锁。
//  如果 向至少一个goroutine发出了抢占请求，则返回true。
func preemptall() bool {
	res := false

	// 遍历 所有 P
	for _, _p_ := range allp {

		// 如果当前 P 不是 running 状态, 则跳过
		if _p_.status != _Prunning {
			continue
		}

		// 尝试 停止 P 中 Grunning 的 G
		if preemptone(_p_) {
			res = true
		}
	}
	return res
}

// Tell the goroutine running on processor P to stop.
// This function is purely best-effort. It can incorrectly fail to inform the
// goroutine. It can send inform the wrong goroutine. Even if it informs the
// correct goroutine, that goroutine might ignore the request if it is
// simultaneously executing newstack.
// No lock needs to be held.
// Returns true if preemption request was issued.
// The actual preemption will happen at some point in the future
// and will be indicated by the gp->status no longer being
// Grunning
//
// todo 抢占P
//
//	告诉处理器	P	上运行的goroutine停止。
//	此功能纯粹是尽力而为。 它可能会错误地无法通知goroutine。
// 	它可以发送通知错误的goroutine。
// 	即使它通知了正确的goroutine，该goroutine如果同时执行newstack也可能会忽略该请求。
//
//	无需持有任何锁。
//	如果发出了抢占请求，则返回true。
//	实际的抢占将在将来的某个时刻发生，并且将由 gp-> status指示，不再是[Grunning] 状态
func preemptone(_p_ *p) bool {
	mp := _p_.m.ptr()
	if mp == nil || mp == getg().m {
		return false
	}
	gp := mp.curg
	if gp == nil || gp == mp.g0 {
		return false
	}

	gp.preempt = true  // 设置g.preempt = true

	// Every call in a go routine checks for stack overflow by
	// comparing the current stack pointer to gp->stackguard0.
	// Setting gp->stackguard0 to StackPreempt folds
	// preemption into the normal stack overflow check.
	gp.stackguard0 = stackPreempt  // 设置g.stackguard0 = stackPreempt
	/**
	todo 为什么设置了stackguard就可以实现抢占?

			因为这个值用于检查当前栈空间是否足够, go函数 的开头会比对这个值判断是否需要扩张栈.
			stackPreempt是一个特殊的常量, 它的值会比任何的栈地址都要大, 检查时一定会触发栈扩张.

	栈扩张调用的是morestack_noctxt函数, morestack_noctxt函数清空rdx寄存器并调用morestack函数.
	 */

	// Request an async preemption of this P.
	if preemptMSupported && debug.asyncpreemptoff == 0 {
		_p_.preempt = true
		preemptM(mp)
	}

	return true
}

var starttime int64

func schedtrace(detailed bool) {
	now := nanotime()
	if starttime == 0 {
		starttime = now
	}

	lock(&sched.lock)
	print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle, " threads=", mcount(), " spinningthreads=", sched.nmspinning, " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
	if detailed {
		print(" gcwaiting=", sched.gcwaiting, " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait, "\n")
	}
	// We must be careful while reading data from P's, M's and G's.
	// Even if we hold schedlock, most data can be changed concurrently.
	// E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
	for i, _p_ := range allp {
		mp := _p_.m.ptr()
		h := atomic.Load(&_p_.runqhead)
		t := atomic.Load(&_p_.runqtail)
		if detailed {
			id := int64(-1)
			if mp != nil {
				id = mp.id
			}
			print("  P", i, ": status=", _p_.status, " schedtick=", _p_.schedtick, " syscalltick=", _p_.syscalltick, " m=", id, " runqsize=", t-h, " gfreecnt=", _p_.gFree.n, " timerslen=", len(_p_.timers), "\n")
		} else {
			// In non-detailed mode format lengths of per-P run queues as:
			// [len1 len2 len3 len4]
			print(" ")
			if i == 0 {
				print("[")
			}
			print(t - h)
			if i == len(allp)-1 {
				print("]\n")
			}
		}
	}

	if !detailed {
		unlock(&sched.lock)
		return
	}

	for mp := allm; mp != nil; mp = mp.alllink {
		_p_ := mp.p.ptr()
		gp := mp.curg
		lockedg := mp.lockedg.ptr()
		id1 := int32(-1)
		if _p_ != nil {
			id1 = _p_.id
		}
		id2 := int64(-1)
		if gp != nil {
			id2 = gp.goid
		}
		id3 := int64(-1)
		if lockedg != nil {
			id3 = lockedg.goid
		}
		print("  M", mp.id, ": p=", id1, " curg=", id2, " mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, ""+" locks=", mp.locks, " dying=", mp.dying, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=", id3, "\n")
	}

	lock(&allglock)
	for gi := 0; gi < len(allgs); gi++ {
		gp := allgs[gi]
		mp := gp.m
		lockedm := gp.lockedm.ptr()
		id1 := int64(-1)
		if mp != nil {
			id1 = mp.id
		}
		id2 := int64(-1)
		if lockedm != nil {
			id2 = lockedm.id
		}
		print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason.String(), ") m=", id1, " lockedm=", id2, "\n")
	}
	unlock(&allglock)
	unlock(&sched.lock)
}

// schedEnableUser enables or disables the scheduling of user
// goroutines.
//
// This does not stop already running user goroutines, so the caller
// should first stop the world when disabling user goroutines.
func schedEnableUser(enable bool) {
	lock(&sched.lock)
	if sched.disable.user == !enable {
		unlock(&sched.lock)
		return
	}
	sched.disable.user = !enable
	if enable {
		n := sched.disable.n
		sched.disable.n = 0
		globrunqputbatch(&sched.disable.runnable, n)
		unlock(&sched.lock)
		for ; n != 0 && sched.npidle != 0; n-- {
			startm(nil, false)
		}
	} else {
		unlock(&sched.lock)
	}
}

// schedEnabled reports whether gp should be scheduled. It returns
// false is scheduling of gp is disabled.
func schedEnabled(gp *g) bool {
	if sched.disable.user {
		return isSystemGoroutine(gp, true)
	}
	return true
}

// Put mp on midle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//
//
// 将 M 放在  全局 schedt 的 空闲 M 队列
//
// 必须是 加锁操作
//
//		可能在STW期间运行，因此不允许写入障碍   (STW 时,  写屏障 肯定已经 关闭了)
//
//
//go:nowritebarrierrec
func mput(mp *m) {
	mp.schedlink = sched.midle
	sched.midle.set(mp)
	sched.nmidle++
	checkdead()
}

// Try to get an m from midle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func mget() *m {
	mp := sched.midle.ptr()
	if mp != nil {
		sched.midle = mp.schedlink
		sched.nmidle--
	}
	return mp
}

// Put gp on the global runnable queue.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func globrunqput(gp *g) {
	sched.runq.pushBack(gp)
	sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
	sched.runq.push(gp)
	sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// This clears *batch.
// Sched must be locked.
//
//	将一批可运行的goroutine放到 schedt 的 runq 中
//	这会清除* batch
//	必须锁定 schedt 实例
func globrunqputbatch(batch *gQueue, n int32) {
	sched.runq.pushBackAll(*batch)
	sched.runqsize += n
	*batch = gQueue{}  // 清空 batch 引用  (切断 batch 的引用指向)
}

// Try get a batch of G's from the global runnable queue.
// Sched must be locked.
func globrunqget(_p_ *p, max int32) *g {
	if sched.runqsize == 0 {
		return nil
	}

	n := sched.runqsize/gomaxprocs + 1
	if n > sched.runqsize {
		n = sched.runqsize
	}
	if max > 0 && n > max {
		n = max
	}
	if n > int32(len(_p_.runq))/2 {
		n = int32(len(_p_.runq)) / 2
	}

	sched.runqsize -= n

	gp := sched.runq.pop()
	n--
	for ; n > 0; n-- {
		gp1 := sched.runq.pop()
		runqput(_p_, gp1, false)
	}
	return gp
}

// Put p to on _Pidle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func pidleput(_p_ *p) {
	if !runqempty(_p_) {
		throw("pidleput: P has non-empty run queue")
	}
	_p_.link = sched.pidle
	sched.pidle.set(_p_)
	atomic.Xadd(&sched.npidle, 1) // TODO: fast atomic
}

// Try get a p from _Pidle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//
//尝试从_Pidle列表中获取p。
//必须锁定Sched。
//可能在STW期间运行，因此不允许写入障碍。
//
//go:nowritebarrierrec
func pidleget() *p {
	_p_ := sched.pidle.ptr()
	if _p_ != nil {
		sched.pidle = _p_.link
		atomic.Xadd(&sched.npidle, -1) // TODO: fast atomic
	}
	return _p_
}

// runqempty reports whether _p_ has no Gs on its local run queue.
// It never returns true spuriously.
func runqempty(_p_ *p) bool {
	// Defend against a race where 1) _p_ has G1 in runqnext but runqhead == runqtail,
	// 2) runqput on _p_ kicks G1 to the runq, 3) runqget on _p_ empties runqnext.
	// Simply observing that runqhead == runqtail and then observing that runqnext == nil
	// does not mean the queue is empty.
	for {
		head := atomic.Load(&_p_.runqhead)
		tail := atomic.Load(&_p_.runqtail)
		runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&_p_.runnext)))
		if tail == atomic.Load(&_p_.runqtail) {
			return head == tail && runnext == 0
		}
	}
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
const randomizeScheduler = raceenabled

// runqput tries to put g on the local runnable queue.
// If next is false, runqput adds g to the tail of the runnable queue.
// If next is true, runqput puts g in the _p_.runnext slot.
// If the run queue is full, runnext puts g on the global queue.
// Executed only by the owner P.
//
// `runqput()` 尝试将g放置在 P 的 runq队列中。
//
//			如果next为false，则runqput将g添加到可运行队列的尾部。
//			如果next为true，则runqput将g放入_p_.runnext插槽中。
//			如果 runq队列已满，则runnext将g放入全局队列。
//			仅由所有者P执行。
//
func runqput(_p_ *p, gp *g, next bool) {
	if randomizeScheduler && next && fastrand()%2 == 0 {
		next = false
	}

	if next {
	retryNext:
		oldnext := _p_.runnext

		// 使用 新进来的 g  替换掉 P 的 runnext 位置中的g
		if !_p_.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
			goto retryNext
		}

		// 如果 P 的 runnext 中的 g 为空, 那么结束
		if oldnext == 0 {
			return
		}
		// Kick the old runnext out to the regular run queue.   将旧的runnext踢出常规 runq
		//
		// 这里先获取 原来 P的 runnext位置 G 的指针
		gp = oldnext.ptr()
	}

retry:

	// 获取 P 的 runq 头 地址
	h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with consumers
	// 获取 P 的 runq 尾 地址
	t := _p_.runqtail

	// 地址的长度间距 < runq中的 G的个数时,  将旧的 runnext 位置的 G 插入到 特定位置
	if t-h < uint32(len(_p_.runq)) {
		_p_.runq[t%uint32(len(_p_.runq))].set(gp)
		atomic.StoreRel(&_p_.runqtail, t+1) // store-release, makes the item available for consumption
		return
	}
	if runqputslow(_p_, gp, h, t) {   // 将一批可运行的goroutine放到 schedt 的 runq 中  todo 里面有判断的, 不一定会真的 做转移哦
		return
	}
	// the queue is not full, now the put above must succeed    队列未满，现在必须成功放置
	goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// Executed only by the owner P.
//
//	将g和 P的 runq中的一批 g 放到  schedt 的 runq 中
//	该函数仅由所有者P执行
func runqputslow(_p_ *p, gp *g, h, t uint32) bool {

	// 初始化一个 batch 队列, 用来装一批 g
	var batch [len(_p_.runq)/2 + 1]*g

	// First, grab a batch from local queue.   首先, 从 P 的 runq队列中抓取一批 G
	n := t - h
	n = n / 2
	if n != uint32(len(_p_.runq)/2) {
		throw("runqputslow: queue is not full")
	}
	for i := uint32(0); i < n; i++ {
		batch[i] = _p_.runq[(h+i)%uint32(len(_p_.runq))].ptr()
	}
	if !atomic.CasRel(&_p_.runqhead, h, h+n) { // cas-release, commits consume   cas-release，提交消耗  todo 这个决定是否真的 将 P 中runq的一批 G 放到  schedt 的runq 中哦
		return false
	}
	batch[n] = gp  // 最后将 入参的 G 也加入 batch 队列


	// 是否需要随机打散下顺序
	if randomizeScheduler {
		for i := uint32(1); i <= n; i++ {
			j := fastrandn(i + 1)
			batch[i], batch[j] = batch[j], batch[i]
		}
	}

	// Link the goroutines.   将 batch 列表中的 G 连接起来
	for i := uint32(0); i < n; i++ {
		batch[i].schedlink.set(batch[i+1])
	}

	// 初始化一个 队列实例
	var q gQueue
	q.head.set(batch[0])
	q.tail.set(batch[n])

	// Now put the batch on global queue.   将这个 batch 中的 g 全部放到 schedt 的 runq 队列中
	lock(&sched.lock)
	globrunqputbatch(&q, int32(n+1))  // 将一批可运行的goroutine放到 schedt 的 runq 中
	unlock(&sched.lock)
	return true
}

// Get g from local runnable queue.
// If inheritTime is true, gp should inherit the remaining time in the
// current time slice. Otherwise, it should start a new time slice.
// Executed only by the owner P.
func runqget(_p_ *p) (gp *g, inheritTime bool) {
	// If there's a runnext, it's the next G to run.
	for {
		next := _p_.runnext
		if next == 0 {
			break
		}
		if _p_.runnext.cas(next, 0) {
			return next.ptr(), true
		}
	}

	for {
		h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with other consumers
		t := _p_.runqtail
		if t == h {
			return nil, false
		}
		gp := _p_.runq[h%uint32(len(_p_.runq))].ptr()
		if atomic.CasRel(&_p_.runqhead, h, h+1) { // cas-release, commits consume
			return gp, false
		}
	}
}

// Grabs a batch of goroutines from _p_'s runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
func runqgrab(_p_ *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
	for {
		h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with other consumers
		t := atomic.LoadAcq(&_p_.runqtail) // load-acquire, synchronize with the producer
		n := t - h
		n = n - n/2
		if n == 0 {
			if stealRunNextG {
				// Try to steal from _p_.runnext.
				if next := _p_.runnext; next != 0 {
					if _p_.status == _Prunning {
						// Sleep to ensure that _p_ isn't about to run the g
						// we are about to steal.
						// The important use case here is when the g running
						// on _p_ ready()s another g and then almost
						// immediately blocks. Instead of stealing runnext
						// in this window, back off to give _p_ a chance to
						// schedule runnext. This will avoid thrashing gs
						// between different Ps.
						// A sync chan send/recv takes ~50ns as of time of
						// writing, so 3us gives ~50x overshoot.
						if GOOS != "windows" {
							usleep(3)
						} else {
							// On windows system timer granularity is
							// 1-15ms, which is way too much for this
							// optimization. So just yield.
							osyield()
						}
					}
					if !_p_.runnext.cas(next, 0) {
						continue
					}
					batch[batchHead%uint32(len(batch))] = next
					return 1
				}
			}
			return 0
		}
		if n > uint32(len(_p_.runq)/2) { // read inconsistent h and t
			continue
		}
		for i := uint32(0); i < n; i++ {
			g := _p_.runq[(h+i)%uint32(len(_p_.runq))]
			batch[(batchHead+i)%uint32(len(batch))] = g
		}
		if atomic.CasRel(&_p_.runqhead, h, h+n) { // cas-release, commits consume
			return n
		}
	}
}

//  todo 从其他P中偷G
//
// 将 p2 中的 可执行G 投一部分到 _p_的 runq队列中, 并返回一个 可执行的G
//
// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
func runqsteal(_p_, p2 *p, stealRunNextG bool) *g {
	t := _p_.runqtail
	n := runqgrab(p2, &_p_.runq, t, stealRunNextG)  // 将 p2 中的 可执行G 投一部分到 _p_的 runq队列中
	if n == 0 {
		return nil
	}
	n--
	gp := _p_.runq[(t+n)%uint32(len(_p_.runq))].ptr()
	if n == 0 {
		return gp
	}
	h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with consumers
	if t-h+n >= uint32(len(_p_.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.StoreRel(&_p_.runqtail, t+n) // store-release, makes the item available for consumption
	return gp
}

// A gQueue is a dequeue of Gs linked through g.schedlink. A G can only
// be on one gQueue or gList at a time.
type gQueue struct {
	head guintptr
	tail guintptr
}

// empty reports whether q is empty.
func (q *gQueue) empty() bool {
	return q.head == 0
}

// push adds gp to the head of q.
func (q *gQueue) push(gp *g) {
	gp.schedlink = q.head
	q.head.set(gp)
	if q.tail == 0 {
		q.tail.set(gp)
	}
}

// pushBack adds gp to the tail of q.
func (q *gQueue) pushBack(gp *g) {
	gp.schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink.set(gp)
	} else {
		q.head.set(gp)
	}
	q.tail.set(gp)
}

// pushBackAll adds all Gs in l2 to the tail of q. After this q2 must
// not be used.
func (q *gQueue) pushBackAll(q2 gQueue) {
	if q2.tail == 0 {
		return
	}
	q2.tail.ptr().schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink = q2.head
	} else {
		q.head = q2.head
	}
	q.tail = q2.tail
}

// pop removes and returns the head of queue q. It returns nil if
// q is empty.
func (q *gQueue) pop() *g {
	gp := q.head.ptr()
	if gp != nil {
		q.head = gp.schedlink
		if q.head == 0 {
			q.tail = 0
		}
	}
	return gp
}

// popList takes all Gs in q and returns them as a gList.
func (q *gQueue) popList() gList {
	stack := gList{q.head}
	*q = gQueue{}
	return stack
}

// A gList is a list of Gs linked through g.schedlink. A G can only be
// on one gQueue or gList at a time.
type gList struct {
	head guintptr
}

// empty reports whether l is empty.
func (l *gList) empty() bool {
	return l.head == 0
}

// push adds gp to the head of l.
func (l *gList) push(gp *g) {
	gp.schedlink = l.head
	l.head.set(gp)
}

// pushAll prepends all Gs in q to l.
func (l *gList) pushAll(q gQueue) {
	if !q.empty() {
		q.tail.ptr().schedlink = l.head
		l.head = q.head
	}
}

// pop removes and returns the head of l. If l is empty, it returns nil.
func (l *gList) pop() *g {
	gp := l.head.ptr()
	if gp != nil {
		l.head = gp.schedlink
	}
	return gp
}

//go:linkname setMaxThreads runtime/debug.setMaxThreads
func setMaxThreads(in int) (out int) {
	lock(&sched.lock)
	out = int(sched.maxmcount)
	if in > 0x7fffffff { // MaxInt32
		sched.maxmcount = 0x7fffffff
	} else {
		sched.maxmcount = int32(in)
	}
	checkmcount()
	unlock(&sched.lock)
	return
}

func haveexperiment(name string) bool {
	if name == "framepointer" {
		return framepointer_enabled // set by linker
	}
	x := sys.Goexperiment
	for x != "" {
		xname := ""
		i := index(x, ",")
		if i < 0 {
			xname, x = x, ""
		} else {
			xname, x = x[:i], x[i+1:]
		}
		if xname == name {
			return true
		}
		if len(xname) > 2 && xname[:2] == "no" && xname[2:] == name {
			return false
		}
	}
	return false
}

// todo 固定住 当前 M 和 G
//
// 目的是绑定G和M，并且获取M上的当前P的pid
// 同时，另一个功能是 保证GC不在  绑定  和  解绑  之间发生
//go:nosplit
func procPin() int {
	_g_ := getg()
	mp := _g_.m

	mp.locks++		//  这里gc在开始之前，会获取当期那M，然后判断当前M上的几个参数，其中的一个参数是locks，这个只要大于0，便不会发生GC

	// 这个locks会在调用pin的使用进行自增，以此来保证GC的不触发.
	// todo 同时，有产生了一个问题，pin 的调用能保证GC的不触发，那么在大量pin的调用下，极端情况下，GC是不是不会执行了呢？

	return int(mp.p.ptr().id)  // 返回 PID
}

// todo 取消 固定 当前 M 和 G
//
//go:nosplit
func procUnpin() {
	_g_ := getg()
	_g_.m.locks--
}


// todo sync.runtime_procPin() 函数的实现
//
//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
	return procPin()
}

// todo sync.runtime_procUnpin() 函数的实现
//
//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
	procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
	procUnpin()
}

// todo 当前g是否可以自旋
// Active spinning for sync.Mutex.  为了 sync.Muitex 而作的 主动 自旋
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
	// sync.Mutex is cooperative, so we are conservative with spinning.
	// Spin only few times and only if running on a multicore machine and
	// GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
	// As opposed to runtime mutex we don't do passive spinning here,
	// because there can be work on global runq or on other Ps.
	/**
		sync.Mutex 是协作的，因此我们对旋转保持保守限制:

	 	仅 自旋了几次，并且仅在多核计算机上运行且 GOMAXPROCS> 1 并且 至少有一个其他正在运行的 P 并且本地 runq 为空时 当前 goroutine 才可以做自旋
	 	runtime 的 互斥锁(mutex) 相反，我们在这里不进行被动旋转，因为可以在 全局runq 或 其他P上进行工作
	 */

	 // 【自旋次数 > 4】 或者 【cpu核数 <= 1】 或者 【gomaxprocs <= 1】 时, 不可 自旋了
	if i >= active_spin || ncpu <= 1 || gomaxprocs <= int32(sched.npidle+sched.nmspinning)+1 {
		return false
	}

	// 当前 g 对应的 p中的 runq 不为 空时, 也不能 自旋了
	if p := getg().m.p.ptr(); !runqempty(p) {
		return false
	}
	return true
}

// todo 进行当前g的自旋
//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
func sync_runtime_doSpin() {
	procyield(active_spin_cnt)
}


// 专门做 从其他P的G队列中偷G 的随机控制器
var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
type randomOrder struct {
	count    uint32
	coprimes []uint32
}

type randomEnum struct {
	i     uint32
	count uint32
	pos   uint32
	inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
	ord.count = count
	ord.coprimes = ord.coprimes[:0]
	for i := uint32(1); i <= count; i++ {
		if gcd(i, count) == 1 {
			ord.coprimes = append(ord.coprimes, i)
		}
	}
}

func (ord *randomOrder) start(i uint32) randomEnum {
	return randomEnum{
		count: ord.count,
		pos:   i % ord.count,
		inc:   ord.coprimes[i%uint32(len(ord.coprimes))],
	}
}

func (enum *randomEnum) done() bool {
	return enum.i == enum.count
}

func (enum *randomEnum) next() {
	enum.i++
	enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
	return enum.pos
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// An initTask represents the set of initializations that need to be done for a package.
// Keep in sync with ../../test/initempty.go:initTask
type initTask struct {
	// TODO: pack the first 3 fields more tightly?
	state uintptr // 0 = uninitialized, 1 = in progress, 2 = done
	ndeps uintptr
	nfns  uintptr
	// followed by ndeps instances of an *initTask, one per package depended on
	// followed by nfns pcs, one per init function to run
}


// runtime.init函数   runtime_init函数  main.init函数  的通用抽象函数
func doInit(t *initTask) {
	switch t.state {
	case 2: // fully initialized
		return
	case 1: // initialization in progress
		throw("recursive call during initialization - linker skew")
	default: // not initialized yet
		t.state = 1 // initialization in progress
		for i := uintptr(0); i < t.ndeps; i++ {
			p := add(unsafe.Pointer(t), (3+i)*sys.PtrSize)
			t2 := *(**initTask)(p)
			doInit(t2)
		}
		for i := uintptr(0); i < t.nfns; i++ {
			p := add(unsafe.Pointer(t), (3+t.ndeps+i)*sys.PtrSize)
			f := *(*func())(unsafe.Pointer(&p))
			f()
		}
		t.state = 2 // initialization done
	}
}
