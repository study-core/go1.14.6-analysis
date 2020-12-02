// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	maxAlign  = 8
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

// chan 类型结构定义
type hchan struct {

	// 当前队列中的元素数量
	qcount   uint           // total data in the queue
	// 队列可以容纳的元素数量, 如果为0表示这个channel无缓冲区
	dataqsiz uint           // size of the circular queue
	// 队列的缓冲区, 结构是环形队列  (无缓冲区的 chan 的 buf值为 nil)
	buf      unsafe.Pointer // points to an array of dataqsiz elements
	// 单个元素的大小
	elemsize uint16
	// 是否已关闭 标识  0: 未关闭,  1: 已关闭
	closed   uint32
	// 元素的类型, 判断是否 调用 【写屏障】 时使用
	elemtype *_type // element type
	// 发送元素的序号  (在 buf 上的索引)
	sendx    uint   // send index
	// 接收元素的序号  (在 buf 上的索引)
	recvx    uint   // receive index
	// 当前等待从channel  接收数据的 G  的链表 (实际类型是 sudog的链表)
	recvq    waitq  // list of recv waiters
	// 当前等待  发送数据  到channel的 G 的链表 (实际类型是sudog的链表)
	sendq    waitq  // list of send waiters

	// lock protects all fields in hchan, as well as several
	// fields in sudogs blocked on this channel.
	//
	// Do not change another G's status while holding this lock
	// (in particular, do not ready a G), as this can deadlock
	// with stack shrinking.
	//
	// 操作channel时使用的线程锁
	lock mutex
}

// 这是一个 goroutine 的 等待队列 的实现
type waitq struct {
	first *sudog		// 队列头 g
	last  *sudog		// 队列尾 g
}

// 实现了  value.go 的 reflect.makechan()
//
// 创建一个 chan
//
//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

// 创建一个通道  创建一个chan (make(chan type, size))
func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// compiler checks this but be safe.
	if elem.size >= 1<<16 {
		throw("makechan: invalid channel element type")
	}
	if hchanSize%maxAlign != 0 || elem.align > maxAlign {
		throw("makechan: bad alignment")
	}

	mem, overflow := math.MulUintptr(elem.size, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
	// buf points into the same allocation, elemtype is persistent.
	// SudoG's are referenced from their owning thread so they can't be collected.
	// TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
	var c *hchan
	switch {
	case mem == 0:
		// Queue or element size is zero.
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector uses this location for synchronization.
		c.buf = c.raceaddr()
	case elem.ptrdata == 0:
		// Elements do not contain pointers.
		// Allocate hchan and buf in one call.
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// Elements contain pointers.
		c = new(hchan)
		c.buf = mallocgc(mem, elem, true)
	}

	c.elemsize = uint16(elem.size)
	c.elemtype = elem
	c.dataqsiz = uint(size)

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
	}
	return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// entry point for c <- x from compiled code
//
//编译代码中 c <-x 的入口点
//
// 往 chan 中 发送数据 的函数实现
//
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 *
 * 往 chan 中 发送数据 的函数实现
 *
 */
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {

	// ep 为本次发送的 元素

	// 先判断 chan 是否为 nil
	if c == nil {
		if !block {
			return false
		}
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, funcPC(chansend))
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not closed, we observe that the channel is
	// not ready for sending. Each of these observations is a single word-sized read
	// (first c.closed and second c.recvq.first or c.qcount depending on kind of channel).
	// Because a closed channel cannot transition from 'ready for sending' to
	// 'not ready for sending', even if the channel is closed between the two observations,
	// they imply a moment between the two when the channel was both not yet closed
	// and not ready for sending. We behave as if we observed the channel at that moment,
	// and report that the send cannot proceed.
	//
	// It is okay if the reads are reordered here: if we observe that the channel is not
	// ready for sending and then observe that it is not closed, that implies that the
	// channel wasn't closed during the first observation.
	if !block && c.closed == 0 && ((c.dataqsiz == 0 && c.recvq.first == nil) ||
		(c.dataqsiz > 0 && c.qcount == c.dataqsiz)) {
		return false
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// 如果 chan 已经 close 则  send 会抛 panic
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}

	// 检查 channel.recvq 是否有等待中的接收者的G     (如果有, 表示channel无缓冲区或者缓冲区为空)
	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value we want to send
		// directly to the receiver, bypassing the channel buffer (if any).
		//
		// 找到了等待的接收者. 我们绕过通道缓冲区（如果有）将要发送的值直接发送到接收器
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	// 走到这里, 说明 没有 被阻塞的  接收G
	//
	// 则, 判断是否可以把元素放到缓冲区中
	//
	// 如果缓冲区有空余的空间, 则把元素放到缓冲区并从chansend返回
	if c.qcount < c.dataqsiz {
		// Space is available in the channel buffer. Enqueue the element to send.
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			raceacquire(qp)
			racerelease(qp)
		}
		typedmemmove(c.elemtype, qp, ep)   // ep -> qp, 本次发送的数据 -> buf 中目前下标的位置变量指针

		// 步进 buf 的发送数据索引
		c.sendx++
		if c.sendx == c.dataqsiz {
			c.sendx = 0
		}
		c.qcount++
		unlock(&c.lock)
		return true
	}

	if !block {
		unlock(&c.lock)
		return false
	}

	// 走到这里, 说明 无缓冲区或缓冲区已经写满
	//
	// 则, 发送者的G 需要等待

	// Block on the channel. Some receiver will complete our operation for us.
	//
	//  在 chan 上 屏蔽  一些 接收器 将为我们完成操作

	// 等待的g 需要被封装成  sodug

	// 获取当前的g
	gp := getg()
	mysg := acquireSudog()   // 新建一个 sudog
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	//
	// 在 分配 elem 和 将 mysg 排队 到 gp 上之前 没有堆栈拆分

	// 设置 sudog.elem = 指向 发送者 内存的指针
	mysg.elem = ep
	mysg.waitlink = nil

	// 设置 sudog.g = g
	mysg.g = gp
	mysg.isSelect = false

	// 设置 sudog.c = channel
	mysg.c = c

	// 设置 g.waiting = sudog
	gp.waiting = mysg

	// 清空 param 的值  (因为当G被唤醒时, param 会有值, 唤醒时传递的参数)
	gp.param = nil

	// 把 sudog 放入 channel.sendq
	c.sendq.enqueue(mysg)

	// 挂起 当前G
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2)
	// Ensure the value being sent is kept alive until the
	// receiver copies it out. The sudog has a pointer to the
	// stack object, but sudogs aren't considered as roots of the
	// stack tracer.
	//
	//
	// 确保发送的值  保持活动状态，直到接收者将其复制出来. sudog 具有指向堆栈对象的指针，但是sudog不被视为堆栈跟踪器的根
	KeepAlive(ep)

	// someone woke us up.   有人把我们 唤醒了
	//
	// 走到这里, 表示已经 成功发送 或者 channel已关闭
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false

	// 检查 sudog.param 是否为nil, 如果为nil表示channel已关闭, 抛出panic   (因为当G被唤醒时, param 会有值, 唤醒时传递的参数)
	if gp.param == nil {
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		panic(plainError("send on closed channel"))
	}
	gp.param = nil   // 再次清空, 方便下次操作
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	mysg.c = nil
	releaseSudog(mysg)    // 否则 释放 发送者 的 sudog 然后返回

	// 从发送者拿到数据并唤醒了G后, 就可以从chansend返回了
	return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {

	// sg 是 接收者 的 G
	// ep 是 本次发送的元素

	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			qp := chanbuf(c, c.recvx)
			raceacquire(qp)
			racerelease(qp)
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}

	// 如果 sudog.elem 不等于nil, 调用 sendDirect()函数 从发送者直接复制元素
	if sg.elem != nil {
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil

		// 等待 接收的 sudog.elem 是指向接收目标的内存的指针, 如果是 接收目标是 `_` 则elem是nil, 可以省略复制
		// 等待 发送的 sudog.elem 是指向来源目标的内存的指针
	}
	gp := sg.g  // sudog 中的 g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	goready(gp, skip+1)  // 复制后调用 goready() 恢复 接收者的G
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine. Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.

// 从 chan 的 发送者 直接复制元素给 接收者
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src is on our stack, dst is a slot on another stack.

	// Once we read sg.elem out of sg, it will no longer
	// be updated if the destination's stack gets copied (shrunk).
	// So make sure that no preemption points can happen between read & use.
	dst := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// No need for cgo write barrier checks because dst is always
	// Go memory.
	memmove(dst, src, t.size)  // src 拷贝数据 到 dst，   src => dst
}

func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// dst is on our stack or the heap, src is on another stack.
	// The channel is locked, so src will not move during this
	// operation.
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)  // src 拷贝数据 到 dst，   src => dst
}


// 关闭通道   关闭chan   close(chan)  实现
func closechan(c *hchan) {
	if c == nil {
		panic(plainError("close of nil channel"))
	}

	lock(&c.lock)
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, funcPC(closechan))
		racerelease(c.raceaddr())
	}

	c.closed = 1  // 设置channel.closed = 1

	var glist gList

	// release all readers
	for {

		// 枚举channel.recvq, 清零 它们sudog.elem, 设置 sudog.param = nil    (全部唤醒 接收者的G ??)
		sg := c.recvq.dequeue()
		if sg == nil {
			break
		}
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = nil
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}

	// release all writers (they will panic)
	for {

		// 枚举 channel.sendq, 设置 sudog.elem = nil, 设置 sudog.param = nil   (全部唤醒 发送者的G ??)
		sg := c.sendq.dequeue()
		if sg == nil {
			break
		}
		sg.elem = nil
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = nil
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}
	unlock(&c.lock)

	// Ready all Gs now that we've dropped the channel lock.
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		goready(gp, 3)    // 调用 goready()函数 恢复所有 接收者 和 发送者 的G
	}
}

// entry points for <- c from compiled code
//
// 编译代码中  <- c 的 入口点
//
// 从 chan 中接收数据 的函数实现
//
//go:nosplit
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}


// x, ok <- chan 的实现
//
//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

//
// 从 chan 中接收数据 的函数实现
//
// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	// raceenabled: don't need to check ep, as it is always on the stack
	// or is new memory allocated by reflect.

	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	// 先判断 chan 是否为 nil
	if c == nil {
		if !block {
			return
		}
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)  // 挂起 当前G
		throw("unreachable")
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not ready for receiving, we observe that the
	// channel is not closed. Each of these observations is a single word-sized read
	// (first c.sendq.first or c.qcount, and second c.closed).
	// Because a channel cannot be reopened, the later observation of the channel
	// being not closed implies that it was also not closed at the moment of the
	// first observation. We behave as if we observed the channel at that moment
	// and report that the receive cannot proceed.
	//
	// The order of operations is important here: reversing the operations can lead to
	// incorrect behavior when racing with a close.
	if !block && (c.dataqsiz == 0 && c.sendq.first == nil ||
		c.dataqsiz > 0 && atomic.Loaduint(&c.qcount) == 0) &&
		atomic.Load(&c.closed) == 0 {
		return
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	if c.closed != 0 && c.qcount == 0 {
		if raceenabled {
			raceacquire(c.raceaddr())
		}
		unlock(&c.lock)
		if ep != nil {
			typedmemclr(c.elemtype, ep)
		}

		// todo 当尝试从一个已经 close 的 chan 读数据的时候，返回 （selected=true, received=false），我们通过 received = false 即可知道 channel 是否 close
		//
		//  就是 会返回 a, ok := <- chan  =>  a == nil,  ok == false
		return true, false
	}

	// 检查channel.sendq中是否有等待中的 发送者的G   todo 注意, 这里 可能 chan已经 close
	if sg := c.sendq.dequeue(); sg != nil {
		// Found a waiting sender. If buffer is size 0, receive value
		// directly from sender. Otherwise, receive from head of queue
		// and add sender's value to the tail of the queue (both map to
		// the same buffer slot because the queue is full).
		//
		//
		// 找到了一个等待发送者. 表示 channel 无缓冲区  或者  缓冲区已满, 这两种情况需要分别处理(为了保证入出队顺序一致)
		recv(c, sg, ep, func() { unlock(&c.lock) }, 3)  // todo recv() 里面最终调用 recvDirect() 函数把元素直接复制 给 接收者

		// 把数据交给接收者并唤醒了G后, 就可以从chanrecv返回了
		return true, true
	}

	// 走到这里, 说明 chan 是有缓冲的

	// 判断是否可以从缓冲区获取元素
	//
	// 如果缓冲区有元素, 则直接取出该元素并从chanrecv返回
	if c.qcount > 0 {
		// Receive directly from queue
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			raceacquire(qp)
			racerelease(qp)
		}
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp)
		}
		typedmemclr(c.elemtype, qp)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.qcount--
		unlock(&c.lock)
		return true, true
	}

	if !block {
		unlock(&c.lock)
		return false, false
	}

	// 无缓冲区无发送者  或  有缓冲区无元素, 接收者的G 需要等待     走到这, 说明 接收者G 被阻塞了

	// no sender available: block on this channel.

	// 获取当前的g
	gp := getg()
	// 新建一个sudog
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	//
	// 在分配elem 和 将mysg入队到 gp.waitcopy 可以找到它的地方之间没有堆栈拆分

	// 设置 sudog.elem = 指向接收内存的指针   todo  这里可能 拿到的是  chan 已经 close 的 elem, 所以 可能为 nil
	mysg.elem = ep
	mysg.waitlink = nil
	// 设置 g.waiting = sudog
	gp.waiting = mysg

	// 设置 sudog.g = g
	mysg.g = gp
	mysg.isSelect = false
	// 设置 sudog.c = channel
	mysg.c = c
	// 清空 唤醒通知标识位, 唤醒时传递的参数
	gp.param = nil
	// 把 sudog 放入 channel.recvq
	c.recvq.enqueue(mysg)

	// 挂起 当前 G (当前G是接收者)
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2)

	// someone woke us up   有人唤醒了我们

	// 走到 这里, 表示已经 成功接收 或者 channel已关闭
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	closed := gp.param == nil   // 检查sudog.param是否为nil, 如果为nil表示channel已关闭   todo 这一句可以看出, 可以接收 已经关闭的chan
	gp.param = nil
	mysg.c = nil
	releaseSudog(mysg)	// 释放sudog然后返回
	return true, !closed  // 和 发送 不一样的是 接收 不会 因为 chan  closed 了而抛panic, 会通过返回值通知channel已关闭
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
// 1) The value sent by the sender sg is put into the channel
//    and the sender is woken up to go on its merry way.
// 2) The value received by the receiver (the current G) is
//    written to ep.
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {

	// sg 是 发送者 的 G
	// ep 是 本次 需要 接收的元素变量



	// 如果无缓冲区, 调用 recvDirect() 函数把元素直接复制给接收者
	if c.dataqsiz == 0 {
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			// copy data from sender
			recvDirect(c.elemtype, sg, ep)  // todo 调用 recvDirect() 函数把元素直接复制给接收者
		}

	// 如果有缓冲区代表缓冲区已满
	} else {
		// Queue is full. Take the item at the
		// head of the queue. Make the sender enqueue
		// its item at the tail of the queue. Since the
		// queue is full, those are both the same slot.

		qp := chanbuf(c, c.recvx)
		if raceenabled {
			raceacquire(qp)
			racerelease(qp)
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}

		// copy data from queue to receiver  将数据从 buf 复制到 接收者
		if ep != nil {

			// 把 buf 中下一个要出队的元素直接复制给接收者   (qp -> ep,  buf中的数据 -> 本次需要接收数据的变量指针, 该指针会直接转交给 接收者)
			typedmemmove(c.elemtype, ep, qp)
		}

		// 把发送的元素复制到队列中刚才出队的位置
		//
		// copy data from sender to queue   将数据从 发送者 复制到 buf 刚刚的变量指针处  (sg.elem -> qp,  发送者的 数据 -> buf中刚刚出队的数据指针)
		typedmemmove(c.elemtype, qp, sg.elem)

		// 步进 buf 的接收指针索引
		c.recvx++

		// 调整 发送序号 和 接收序号
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	goready(gp, skip+1)   // 复制后, 调用 goready() 恢复 之前因为阻塞被挂起的接收者的G

}

// 回调 用来唤醒 参与 chan 操作被阻塞的 G
func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	gp.activeStackChans = true
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
//
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbrecv(&v, c) {
//		... foo
//	} else {
//		... bar
//	}
//
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected bool) {
	selected, _ = chanrecv(c, elem, false)
	return
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if c != nil && selectnbrecv2(&v, &ok, c) {
//		... foo
//	} else {
//		... bar
//	}
//
func selectnbrecv2(elem unsafe.Pointer, received *bool, c *hchan) (selected bool) {
	// TODO(khr): just return 2 values from this function, now that it is in Go.
	selected, *received = chanrecv(c, elem, false)
	return
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil {
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

func (q *waitq) dequeue() *sudog {
	for {
		sgp := q.first
		if sgp == nil {
			return nil
		}
		y := sgp.next
		if y == nil {
			q.first = nil
			q.last = nil
		} else {
			y.prev = nil
			q.first = y
			sgp.next = nil // mark as removed (see dequeueSudog)
		}

		// if a goroutine was put on this queue because of a
		// select, there is a small window between the goroutine
		// being woken up by a different case and it grabbing the
		// channel locks. Once it has the lock
		// it removes itself from the queue, so we won't see it after that.
		// We use a flag in the G struct to tell us when someone
		// else has won the race to signal this goroutine but the goroutine
		// hasn't removed itself from the queue yet.
		if sgp.isSelect && !atomic.Cas(&sgp.g.selectDone, 0, 1) {
			continue
		}

		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}
