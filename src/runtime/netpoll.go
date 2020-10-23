// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd js,wasm linux netbsd openbsd solaris windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// todo 网络轮询器 定义
//
//   (参考: https://draveness.me/golang/docs/part3-runtime/ch06-concurrency/golang-netpoller/ )
//
//      网络轮询器  实际上就是对 I/O 多路复用技术的封装
//
//   	网络轮询器就是 Go 语言运行时用来处理 I/O 操作的关键组件，它使用了操作系统提供的 I/O 多路复用机制增强程序的并发处理能力.
//
//
//
//	网络轮询器的实现原理：
//
//				1、网络轮询器的初始化.  (网络轮询器的初始化会使用 runtime.poll_runtime_pollServerInit 和 runtime.netpollGenericInit 两个函数)  poll_runtime_pollServerInit 也是调了 netpollGenericInit
//				2、如何向网络轮询器中加入待监控的任务.
//				3、如何从网络轮询器中获取触发的事件.
//
//	上述三个过程包含了网络轮询器相关的方方面面，能够让我们对其实现有完整的理解
//
//
//	文件 I/O、网络 I/O   以及   计时器(Timer、Ticker)    都依赖网络轮询器，所以 Go 语言会通过以下两条不同路径初始化网络轮询器：
//
//	internal/poll.pollDesc.init: 		通过 net.netFD.init 和 os.newFile 初始化 【网络 I/O】 和  【文件 I/O】 的轮询信息时
//	runtime.doaddtimer: 				向处理器中增加新的计时器时
//
//
// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue/port/AIX/Windows)
// must define the following functions:
//
//  todo 跨平台实现, 每种平台的实现都必须 定义下面的 4 个 函数
//
//		todo 初始化网络轮询器，通过 sync.Once 和 netpollInited 变量保证函数只会调用一次
// func netpollinit()
//     Initialize the poller. Only called once.
//
//		todo 监听文件描述符上的边缘触发事件，创建事件并加入监听
// func netpollopen(fd uintptr, pd *pollDesc) int32
//     Arm edge-triggered notifications for fd. The pd argument is to pass
//     back to netpollready when fd is ready. Return an errno value.
//
//		todo 轮询网络并返回一组已经准备就绪的 Goroutine，传入的参数会决定它的行为
//
//				如果参数 < 0，无限期等待文件描述符就绪.
//				如果参数 == 0，非阻塞地轮询网络.
//				如果参数 > 0，阻塞特定时间轮询网络.
//
// func netpoll(delta int64) gList
//     Poll the network. If delta < 0, block indefinitely. If delta == 0,
//     poll without blocking. If delta > 0, block for up to delta nanoseconds.
//     Return a list of goroutines built by calling netpollready.
//
//		todo 唤醒网络轮询器，例如：计时器向前修改时间时会通过该函数中断网络轮询器
// func netpollBreak()
//     Wake up the network poller, assumed to be blocked in netpoll.
//
//		todo 判断文件描述符是否被轮询器使用
// func netpollIsPollDescriptor(fd uintptr) bool
//     Reports whether fd is a file descriptor used by the poller.

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
// pdReady - io readiness notification is pending;
//           a goroutine consumes the notification by changing the state to nil.
// pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//          the goroutine commits to park by changing the state to G pointer,
//          or, alternatively, concurrent io notification changes the state to READY,
//          or, alternatively, concurrent timeout/close changes the state to nil.
// G pointer - the goroutine is blocked on the semaphore;
//             io notification or timeout/close changes the state to READY or nil respectively
//             and unparks the goroutine.
// nil - nothing of the above.
const (
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024

// 操作系统的文件描述符    (网轮询器描述符 定义)
//
//			不在 heap 上分配
//
//			Go 语言 网络轮询器 会监听这个 runtime.pollDesc 结构体的状态
//
//
//		该结构体中包含用于监控可读和可写状态的变量，我们按照功能将它们分成以下四组：
//
//   				rseq 和 wseq：  表示文件描述符被重用  或者 计时器被重置
//   				rg 和 wg：		表示二进制的信号量，可能为 pdReady、pdWait、等待文件描述符可读 或者 可写的 Goroutine 以及 nil
//   				rd 和 wd： 		等待文件描述符可读 或者 可写的 截止日期
//   				rt 和 wt： 		用于等待文件描述符的计时器
//
// 		除了上述八个变量之外，该结构体中还保存了   用于保护数据的互斥锁、文件描述符。runtime.pollDesc 结构体会使用 link 字段串联成一个链表存储在 runtime.pollCache 中
//
// Network poller descriptor.
//
// No heap pointers.
//
//go:notinheap
type pollDesc struct {
	link *pollDesc // in pollcache, protected by pollcache.lock

	// The lock protects pollOpen, pollSetDeadline, pollUnblock and deadlineimpl operations.
	// This fully covers seq, rt and wt variables. fd is constant throughout the PollDesc lifetime.
	// pollReset, pollWait, pollWaitCanceled and runtime·netpollready (IO readiness notification)
	// proceed w/o taking the lock. So closing, everr, rg, rd, wg and wd are manipulated
	// in a lock-free way by all operations.
	// NOTE(dvyukov): the following code uses uintptr to store *g (rg/wg),
	// that will blow up when GC starts moving objects.
	lock    mutex // protects the following fields

	// 文件描述符
	fd      uintptr
	closing bool
	everr   bool    // marks event scanning error happened
	user    uint32  // user settable cookie

	//  下面 8 个变量 被分为 4 组

	rseq    uintptr // protects from stale read timers
	rg      uintptr // pdReady, pdWait, G waiting for read or nil    	todo 等待 read 操作的G
	rt      timer   // read deadline timer (set if rt.f != nil)
	rd      int64   // read deadline
	wseq    uintptr // protects from stale write timers
	wg      uintptr // pdReady, pdWait, G waiting for write or nil		todo 等待 write 操作的G
	wt      timer   // write deadline timer
	wd      int64   // write deadline
}


// 缓存 pollDesc 的缓存
type pollCache struct {
	lock  mutex
	first *pollDesc
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
}

var (
	netpollInitLock mutex
	netpollInited   uint32


	// 运行时包中的全局变量，该结构体中包含一个用于保护轮询数据的互斥锁和链表头
	pollcache      pollCache
	netpollWaiters uint32
)

//go:linkname poll_runtime_pollServerInit internal/poll.runtime_pollServerInit
func poll_runtime_pollServerInit() {
	netpollGenericInit()
}

// 初始化 网络轮询器
func netpollGenericInit() {

	// 调用平台上特定实现的 runtime.netpollinit() 函数，即 Linux 上的 epoll，它主要做了以下几件事情：
	//
	// 			1、调用 epollcreate1 创建一个新的 epoll 文件描述符，这个文件描述符会在整个程序的生命周期中使用.
	// 		 	2、通过 runtime.nonblockingPipe 创建一个用于通信的管道.
	// 			3、使用 epollctl 将用于读取数据的文件描述符打包成 epollevent 事件加入监听.

	if atomic.Load(&netpollInited) == 0 {
		lock(&netpollInitLock)
		if netpollInited == 0 {
			netpollinit()
			atomic.Store(&netpollInited, 1)
		}
		unlock(&netpollInitLock)
	}
}

func netpollinited() bool {
	return atomic.Load(&netpollInited) != 0
}

//go:linkname poll_runtime_isPollServerDescriptor internal/poll.runtime_isPollServerDescriptor

// poll_runtime_isPollServerDescriptor reports whether fd is a
// descriptor being used by netpoll.
func poll_runtime_isPollServerDescriptor(fd uintptr) bool {
	return netpollIsPollDescriptor(fd)
}

// 重置轮询信息 runtime.pollDesc 并调用 runtime.netpollopen() 初始化轮询事件
//
//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (*pollDesc, int) {

	// 从全局的 pollCache 的 pollDesc链表中获取 头部.
	pd := pollcache.alloc()
	lock(&pd.lock)
	if pd.wg != 0 && pd.wg != pdReady {
		throw("runtime: blocked write on free polldesc")
	}
	if pd.rg != 0 && pd.rg != pdReady {
		throw("runtime: blocked read on free polldesc")
	}

	// 将 pollDesc 的各个字段 重置
	pd.fd = fd
	pd.closing = false
	pd.everr = false
	pd.rseq++
	pd.rg = 0
	pd.rd = 0
	pd.wseq++
	pd.wg = 0
	pd.wd = 0
	unlock(&pd.lock)

	var errno int32
	errno = netpollopen(fd, pd)  // 初始化轮询事件
	return pd, int(errno)
}

//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(pd *pollDesc) {
	if !pd.closing {
		throw("runtime: close polldesc w/o unblock")
	}
	if pd.wg != 0 && pd.wg != pdReady {
		throw("runtime: blocked write on closing polldesc")
	}
	if pd.rg != 0 && pd.rg != pdReady {
		throw("runtime: blocked read on closing polldesc")
	}
	netpollclose(pd.fd)
	pollcache.free(pd)
}

// Go 语言运行时会调用 runtime.pollCache.free 方法  释放已经用完的 runtime.pollDesc 结构，它会直接将结构体插入链表的最前面
func (c *pollCache) free(pd *pollDesc) {

	// 将  pollDesc 放回 cache 的链表的 头部
	//
	//  该方法 没有重置 runtime.pollDesc 结构体中的字段，该结构体被重复利用时才会由 runtime.poll_runtime_pollOpen 函数重置
	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(pd *pollDesc, mode int) int {
	err := netpollcheckerr(pd, int32(mode))
	if err != 0 {
		return err
	}
	if mode == 'r' {
		pd.rg = 0
	} else if mode == 'w' {
		pd.wg = 0
	}
	return 0
}

// Goroutine 让出线程并等待读写事件
//
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(pd *pollDesc, mode int) int {
	err := netpollcheckerr(pd, int32(mode))
	if err != 0 {
		return err
	}
	// As for now only Solaris, illumos, and AIX use level-triggered IO.
	if GOOS == "solaris" || GOOS == "illumos" || GOOS == "aix" {
		netpollarm(pd, mode)
	}

	// 当我们在 文件描述符 上执行读写操作时，如果文件描述符 不可读 或者 不可写，
	// 当前 Goroutine 就会执行 runtime.poll_runtime_pollWait() 检查 runtime.pollDesc 的状态并调用 runtime.netpollblock() 等待文件描述符的 可读 或者 可写.
	for !netpollblock(pd, int32(mode), false) {
		err = netpollcheckerr(pd, int32(mode))
		if err != 0 {
			return err
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
	}
	return 0
}

//go:linkname poll_runtime_pollWaitCanceled internal/poll.runtime_pollWaitCanceled
func poll_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	for !netpollblock(pd, int32(mode), true) {
	}
}
// 截止日期
//
//
// 网络轮询器  和   计时器 (Timer 和 Ticker)  的关系非常紧密，这不仅仅是因为 [ 网络轮询器负责计时器的唤醒 ]，
// 				还因为 [文件] 和 [网络 I/O]  的截止日期 也由网络轮询器负责处理。
// 				截止日期在 I/O 操作中，尤其是网络调用中很关键，网络请求存在很高的不确定因素，
// 				我们需要设置一个截止日期保证程序的正常运行，这时就需要用到网络轮询器中的 runtime.poll_runtime_pollSetDeadline() 函数.
//
//
//go:linkname poll_runtime_pollSetDeadline internal/poll.runtime_pollSetDeadline
func poll_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {


	// 先使用截止日期计算出过期的时间点，然后根据 runtime.pollDesc 的状态做出以下不同的处理：
	//
	//  1、如果结构体中的 计时器 没有设置执行的函数时，该函数会设置计时器到期后执行的函数、传入的参数并调用 runtime.resettimer() 重置计时器
	//  2、如果结构体的读截止日期已经被改变，我们会根据新的截止日期做出不同的处理：
	//  		a、如果 新的截止日期 > 0，调用 runtime.modtimer() 修改计时器
	//  		b、如果 新的截止日期 < 0，调用 runtime.deltimer() 删除计时器
	//
	//  在 runtime.poll_runtime_pollSetDeadline() 函数的最后，会重新检查轮询信息中存储的截止日期.

	lock(&pd.lock)
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	rd0, wd0 := pd.rd, pd.wd
	combo0 := rd0 > 0 && rd0 == wd0
	if d > 0 {
		d += nanotime()
		if d <= 0 {
			// If the user has a deadline in the future, but the delay calculation
			// overflows, then set the deadline to the maximum possible value.
			d = 1<<63 - 1
		}
	}
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}
	combo := pd.rd > 0 && pd.rd == pd.wd
	rtf := netpollReadDeadline
	if combo {
		rtf = netpollDeadline
	}
	if pd.rt.f == nil {
		if pd.rd > 0 {
			pd.rt.f = rtf
			// Copy current seq into the timer arg.
			// Timer func will check the seq against current descriptor seq,
			// if they differ the descriptor was reused or timers were reset.
			pd.rt.arg = pd
			pd.rt.seq = pd.rseq
			resettimer(&pd.rt, pd.rd)
		}
	} else if pd.rd != rd0 || combo != combo0 {
		pd.rseq++ // invalidate current timers
		if pd.rd > 0 {
			modtimer(&pd.rt, pd.rd, 0, rtf, pd, pd.rseq)
		} else {
			deltimer(&pd.rt)
			pd.rt.f = nil
		}
	}
	if pd.wt.f == nil {
		if pd.wd > 0 && !combo {
			pd.wt.f = netpollWriteDeadline
			pd.wt.arg = pd
			pd.wt.seq = pd.wseq
			resettimer(&pd.wt, pd.wd)
		}
	} else if pd.wd != wd0 || combo != combo0 {
		pd.wseq++ // invalidate current timers
		if pd.wd > 0 && !combo {
			modtimer(&pd.wt, pd.wd, 0, netpollWriteDeadline, pd, pd.wseq)
		} else {
			deltimer(&pd.wt)
			pd.wt.f = nil
		}
	}
	// If we set the new deadline in the past, unblock currently pending IO if any.
	var rg, wg *g
	if pd.rd < 0 || pd.wd < 0 {
		atomic.StorepNoWB(noescape(unsafe.Pointer(&wg)), nil) // full memory barrier between stores to rd/wd and load of rg/wg in netpollunblock
		if pd.rd < 0 {
			rg = netpollunblock(pd, 'r', false)
		}
		if pd.wd < 0 {
			wg = netpollunblock(pd, 'w', false)
		}
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)   // 如果是 等待 read 的G, 则唤醒
	}
	if wg != nil {
		netpollgoready(wg, 3)  // 如果是 等待 write 的G, 则唤醒
	}

	// 在 runtime.poll_runtime_pollSetDeadline() 函数中直接调用 runtime.netpollgoready() 是相对比较特殊的情况
	//
	// 			在正常情况下，运行时 都会在  计时器到期时  调用 runtime.netpollDeadline()、runtime.netpollReadDeadline() 和 runtime.netpollWriteDeadline() 三个函数
	//			但是上述三个函数 最终也都会通过 runtime.netpolldeadlineimpl 调用 runtime.netpollgoready() 直接唤醒相应的 G
	//
	// 		G  在被唤醒之后就会意识到当前的 I/O 操作已经超时，可以根据需要选择  重试请求 或者  中止调用
}

//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock)
	if pd.closing {
		throw("runtime: unblock on closing polldesc")
	}
	pd.closing = true
	pd.rseq++
	pd.wseq++
	var rg, wg *g
	atomic.StorepNoWB(noescape(unsafe.Pointer(&rg)), nil) // full memory barrier between store to closing and read of rg/wg in netpollunblock
	rg = netpollunblock(pd, 'r', false)
	wg = netpollunblock(pd, 'w', false)
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

// netpollready is called by the platform-specific netpoll function.
// It declares that the fd associated with pd is ready for I/O.
// The toRun argument is used to build a list of goroutines to return
// from netpoll. The mode argument is 'r', 'w', or 'r'+'w' to indicate
// whether the fd is ready for reading or writing or both.
//
// This may run while the world is stopped, so write barriers are not allowed.
//go:nowritebarrier
func netpollready(toRun *gList, pd *pollDesc, mode int32) {

	// runtime.netpollunblock() 会在读写事件发生时，将 runtime.pollDesc 中的 读 或者 写 信号量转换成 pdReady 并返回其中存储的 Goroutine.
	// 如果返回的 Goroutine 不会为空，那么该 Goroutine 就会被加入 toRun 列表，运行时会将列表中的全部 Goroutine 加入运行队列并   等待调度器的调度

	var rg, wg *g
	if mode == 'r' || mode == 'r'+'w' {
		rg = netpollunblock(pd, 'r', true)
	}
	if mode == 'w' || mode == 'r'+'w' {
		wg = netpollunblock(pd, 'w', true)
	}
	if rg != nil {
		toRun.push(rg)
	}
	if wg != nil {
		toRun.push(wg)
	}
}

func netpollcheckerr(pd *pollDesc, mode int32) int {
	if pd.closing {
		return 1 // ErrFileClosing or ErrNetClosing
	}
	if (mode == 'r' && pd.rd < 0) || (mode == 'w' && pd.wd < 0) {
		return 2 // ErrTimeout
	}
	// Report an event scanning error only on a read event.
	// An error on a write event will be captured in a subsequent
	// write call that is able to report a more specific error.
	if mode == 'r' && pd.everr {
		return 3 // ErrNotPollable
	}
	return 0
}

func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool {
	r := atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))
	if r {
		// Bump the count of goroutines waiting for the poller.
		// The scheduler uses this to decide whether to block
		// waiting for the poller if there is nothing else to do.
		atomic.Xadd(&netpollWaiters, 1)
	}
	return r
}

func netpollgoready(gp *g, traceskip int) {
	atomic.Xadd(&netpollWaiters, -1)
	goready(gp, traceskip+1)
}

//  runtime.netpollblock() 是 Goroutine 等待 I/O 事件的关键函数
//
// 				它会使用运行时提供的 runtime.gopark() 让出当前线程 M，将 Goroutine 转换到【休眠状态】并等待运行时的唤醒
//
// returns true if IO is ready, or false if timedout or closed
// waitio - wait only for completed IO, ignore errors
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// set the gpp semaphore to WAIT
	for {
		old := *gpp
		if old == pdReady {
			*gpp = 0
			return true
		}
		if old != 0 {
			throw("runtime: double wait")
		}
		if atomic.Casuintptr(gpp, 0, pdWait) {
			break
		}
	}

	// need to recheck error states after setting gpp to WAIT
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, membarrier, load of rg/wg
	if waitio || netpollcheckerr(pd, mode) == 0 {
		// 挂起 正在等待 R/W 操作的 G,  让出 M
		gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceEvGoBlockNet, 5)
	}
	// be careful to not lose concurrent READY notification
	old := atomic.Xchguintptr(gpp, 0)
	if old > pdWait {
		throw("runtime: corrupted polldesc")
	}
	return old == pdReady
}

// 在 读写事件 发生时，将 runtime.pollDesc 中的 读 或者 写 信号量 转换成 pdReady 并返回其中存储的 Goroutine.
// 		如果返回的 Goroutine 不会为空，那么该 Goroutine 就会被加入 toRun 列表，运行时会将列表中的全部 Goroutine 加入运行队列并  等待调度器的调度.
func netpollunblock(pd *pollDesc, mode int32, ioready bool) *g {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	for {
		old := *gpp
		if old == pdReady {
			return nil
		}
		if old == 0 && !ioready {
			// Only set READY for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			return nil
		}
		var new uintptr
		if ioready {
			new = pdReady
		}
		if atomic.Casuintptr(gpp, old, new) {
			if old == pdReady || old == pdWait {
				old = 0
			}
			return (*g)(unsafe.Pointer(old))
		}
	}
}

func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	currentSeq := pd.rseq
	if !read {
		currentSeq = pd.wseq
	}
	if seq != currentSeq {
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}
	var rg *g
	if read {
		if pd.rd <= 0 || pd.rt.f == nil {
			throw("runtime: inconsistent read deadline")
		}
		pd.rd = -1
		atomic.StorepNoWB(unsafe.Pointer(&pd.rt.f), nil) // full memory barrier between store to rd and load of rg in netpollunblock
		rg = netpollunblock(pd, 'r', false)
	}
	var wg *g
	if write {
		if pd.wd <= 0 || pd.wt.f == nil && !read {
			throw("runtime: inconsistent write deadline")
		}
		pd.wd = -1
		atomic.StorepNoWB(unsafe.Pointer(&pd.wt.f), nil) // full memory barrier between store to wd and load of wg in netpollunblock
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 0)    // 如果是 等待 read 的G, 则唤醒
	}
	if wg != nil {
		netpollgoready(wg, 0)   // 如果是 等待 write 的G, 则唤醒
	}
}

func netpollDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}

func netpollReadDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}

func netpollWriteDeadline(arg interface{}, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}


// 运行时会在第一次调用 runtime.pollCache.alloc 方法时初始化总大小约为 4KB 的 runtime.pollDesc 结构体，
// 			runtime.persistentalloc 会保证这些数据结构初始化在【不会触发 GC】的内存中，让这些数据结构只能被内部的 epoll 和 kqueue 模块引用
//
func (c *pollCache) alloc() *pollDesc {
	lock(&c.lock)
	if c.first == nil {
		const pdSize = unsafe.Sizeof(pollDesc{})
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		//
		//必须位于非GC内存中，因为只能从 epoll/kqueue 内部引用
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys) // runtime.persistentalloc 会保证这些数据结构初始化在【不会触发 GC】的内存中，让这些数据结构只能被内部的 epoll 和 kqueue 模块引用
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first
	c.first = pd.link  // 下一个 pollDesc 又变成 新的链表头
	unlock(&c.lock)

	// 每次调用该函数.  全局的 pollCache都会返回 其 链表头还没有被使用的 runtime.pollDesc，这种批量初始化的做法能够 增加网络轮询器的吞吐量
	return pd
}
