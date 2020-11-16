// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go select statements.

import (
	"unsafe"
)

const debugSelect = false

// scase.kind values.
// Known to compiler.
// Changes here must also be made in src/cmd/compile/internal/gc/select.go's walkselectcases.
const (
	caseNil = iota		// 0: 空的 select  case 语句块
	caseRecv			// 1: obj <- chan 的 select case 语句块
	caseSend			// 2: chan <- obj 的 select case 语句块
	caseDefault			// 3: default 的 select case 语句块
)

/**
 todo select case结构 描述符
 编译器已知
 必须在 `src/cmd/internal/gc/select.go` 的 scasetype 中进行更改。
 */
// Select case descriptor.
// Known to compiler.
// Changes here must also be made in src/cmd/internal/gc/select.go's scasetype.
type scase struct {
	c           *hchan         // chan
	elem        unsafe.Pointer // data element
	kind        uint16
	pc          uintptr // race pc (for race detector / msan)
	releasetime int64
}

var (
	chansendpc = funcPC(chansend)
	chanrecvpc = funcPC(chanrecv)
)

func selectsetpc(cas *scase) {
	cas.pc = getcallerpc()
}

func sellock(scases []scase, lockorder []uint16) {
	var c *hchan
	for _, o := range lockorder {
		c0 := scases[o].c
		if c0 != nil && c0 != c {
			c = c0
			lock(&c.lock)  // select 逐个 锁住 chan
		}
	}
}

func selunlock(scases []scase, lockorder []uint16) {
	// We must be very careful here to not touch sel after we have unlocked
	// the last lock, because sel can be freed right after the last unlock.
	// Consider the following situation.
	// First M calls runtime·park() in runtime·selectgo() passing the sel.
	// Once runtime·park() has unlocked the last lock, another M makes
	// the G that calls select runnable again and schedules it for execution.
	// When the G runs on another M, it locks all the locks and frees sel.
	// Now if the first M touches sel, it will access freed memory.
	for i := len(scases) - 1; i >= 0; i-- {
		c := scases[lockorder[i]].c
		if c == nil {
			break
		}
		if i > 0 && c == scases[lockorder[i-1]].c {
			continue // will unlock it on the next iteration
		}
		unlock(&c.lock)  // select 逐个 解锁  chan
	}
}

func selparkcommit(gp *g, _ unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	gp.activeStackChans = true
	// This must not access gp's stack (see gopark). In
	// particular, it must not access the *hselect. That's okay,
	// because by the time this is called, gp.waiting has all
	// channels in lock order.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc && lastc != nil {
			// As soon as we unlock the channel, fields in
			// any sudog with that channel may change,
			// including c and waitlink. Since multiple
			// sudogs may have the same channel, we unlock
			// only after we've passed the last instance
			// of a channel.
			unlock(&lastc.lock) // select 逐个 解锁  chan
		}
		lastc = sg.c
	}
	if lastc != nil {
		unlock(&lastc.lock) // select  解锁  lastc chan
	}
	return true
}

func block() {
	gopark(nil, nil, waitReasonSelectNoCases, traceEvGoStop, 1) // forever
}


// todo  select 关键字 语句块 最终执行函数
//		select 关键字 最终的执行 函数
//
//	入参说明:
//
//		cas0： scase数组的首地址， selectgo()就是从这些scase中找出一个返回
//		order0: 一个两倍 cas0 数组长度的 buffer，保存scase随机序列 pollorder 和 scase 中 channel地址序列 lockorder
//
//
//			pollorder： 每次 selectgo 执行 都会把scase序列打乱， 以达到随机检测case的目的
//			lockorder： 所有case语句中channel序列， 以达到去重防止对channel加锁时重复加锁的目的
//
//	返参返回:
//
//		int： 选中case的编号， 这个case编号跟代码一致.
//		bool: 是否成功从channle中读取了数据， 如果选中的case是从channel中读数据， 则该返回值表示是否读
//
/**
	selectgo 实现 select语句

	cas0 指向 [ncases]scase类型的数组，
	order0指向[2 * ncases]uint16 类型的数组。

	两者都驻留在goroutine的堆栈上（无论selectgo中有任何转义）

	selectgo 返回所选 case 的索引，该索引与其各自的select {recv，send，default}调用的顺序位置相匹配。
	同样，如果所选的 `scase` 是接收操作，它将报告是否接收到一个值
 */
// selectgo implements the select statement.
//
// cas0 points to an array of type [ncases]scase, and order0 points to
// an array of type [2*ncases]uint16. Both reside on the goroutine's
// stack (regardless of any escaping in selectgo).
//
// selectgo returns the index of the chosen scase, which matches the
// ordinal position of its respective select{recv,send,default} call.
// Also, if the chosen scase was a receive operation, it reports whether
// a value was received.
func selectgo(cas0 *scase, order0 *uint16, ncases int) (int, bool) {

	/**
	todo 这个函数做的事是:

			1. 锁定scase语句中所有的channel  todo 好比现在 要抓拍个快照, 则大家的chan 都先暂停操作了

			2. 按照随机顺序检测scase中的channel是否ready

			 		2.1 如果case可读， 则读取channel中数据， 解锁所有的channel， 然后返回(case index, true)
			 		2.2 如果case可写， 则将数据写入channel， 解锁所有的channel， 然后返回(case index, false)
			 		2.3 所有case都未ready， 则解锁所有的channel， 然后返回（default index, false）

			3. 所有case都未ready， 且没有default语句

			 		3.1 将当前协程加入到所有channel的等待队列
			 		3.2 当将协程转入阻塞， 等待被唤醒

			4. 唤醒后返回channel对应的case index

			 		4.1 如果是读操作， 解锁所有的channel， 然后返回(case index, true)
			 		4.2 如果是写操作， 解锁所有的channel， 然后返回(case index, false)
	 */

	if debugSelect {
		print("select: cas0=", cas0, "\n")
	}

	// 取出
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0))

	scases := cas1[:ncases:ncases]
	pollorder := order1[:ncases:ncases]
	lockorder := order1[ncases:][:ncases:ncases]

	// Replace send/receive cases involving nil channels with
	// caseNil so logic below can assume non-nil channel.
	//
	// 用 `caseNil` 代替涉及 `nil channel` 的 `send/receive cases`，因此下面的逻辑可以假定 `non-nil channel`
	//
	// 说白了, 就是先处理掉 nil 的 case, 给个 空结构 `scase{}`的值
	for i := range scases {
		cas := &scases[i]
		if cas.c == nil && cas.kind != caseDefault {
			*cas = scase{}
		}
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()  // 取 原子钟 ??
		for i := 0; i < ncases; i++ {
			scases[i].releasetime = -1
		}
	}

	// The compiler rewrites selects that statically have
	// only 0 or 1 cases plus default into simpler constructs.
	// The only way we can end up with such small sel.ncase
	// values here is for a larger select in which most channels
	// have been nilled out. The general code handles those
	// cases correctly, and they are rare enough not to bother
	// optimizing (and needing to test).

	// generate permuted order  生成排列顺序
	//
	// todo 【超级重要】 每次执行 select 时, 都需要对  所有的 case 做一次 随机洗牌
	for i := 1; i < ncases; i++ {
		j := fastrandn(uint32(i + 1))
		pollorder[i] = pollorder[j]
		pollorder[j] = uint16(i)
	}

	// sort the cases by Hchan address to get the locking order.
	// simple heap sort, to guarantee n log n time and constant stack footprint.
	//
	//	通过 `hchan` 地址 对 case 进行排序以获得锁定顺序
	//	简单的 【堆排序】，以确保 O(N log N) 个时间 和 恒定的堆栈占用量   todo  堆排 主要还是为了 做 chan 去重的
	//
	// todo 【重要】 对 所有的 case 中的 chan 做需要加锁标识,  下面会统一去上锁的
	//						todo 【细节】对 chan 上锁时 需要做去重动作, 因为 可能多个 case 中引用的 chan 是同一个, 不能重复上锁
	for i := 0; i < ncases; i++ {
		j := i
		// Start with the pollorder to permute cases on the same channel.
		c := scases[pollorder[i]].c
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockorder[j] = lockorder[k]
			j = k
		}
		lockorder[j] = pollorder[i]
	}
	for i := ncases - 1; i >= 0; i-- {
		o := lockorder[i]
		c := scases[o].c
		lockorder[i] = lockorder[0]
		j := 0
		for {
			k := j*2 + 1
			if k >= i {
				break
			}
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++
			}
			if c.sortkey() < scases[lockorder[k]].c.sortkey() {
				lockorder[j] = lockorder[k]
				j = k
				continue
			}
			break
		}
		lockorder[j] = o
	}

	if debugSelect {
		for i := 0; i+1 < ncases; i++ {
			if scases[lockorder[i]].c.sortkey() > scases[lockorder[i+1]].c.sortkey() {
				print("i=", i, " x=", lockorder[i], " y=", lockorder[i+1], "\n")
				throw("select: broken sort")
			}
		}
	}

	// lock all the channels involved in the select
	sellock(scases, lockorder) // 锁定 select 中涉及的所有 chan

	var (
		gp     *g
		sg     *sudog
		c      *hchan
		k      *scase
		sglist *sudog
		sgnext *sudog
		qp     unsafe.Pointer
		nextp  **sudog
	)

loop:
	// pass 1 - look for something already waiting
	var dfli int
	var dfl *scase
	var casi int
	var cas *scase
	var recvOK bool

	// todo 开始 逐个 遍历 洗牌 和 chan上锁 后的 所有case
	for i := 0; i < ncases; i++ {
		casi = int(pollorder[i])
		cas = &scases[casi]
		c = cas.c


		// todo 判断当前 case 中 chan 的方向
		switch cas.kind {

		// 0: 空的 select case 语句块
		case caseNil:
			continue


		// 1: obj <- chan 的 select case 语句块
		case caseRecv:
			sg = c.sendq.dequeue()
			if sg != nil {
				goto recv
			}
			if c.qcount > 0 {
				goto bufrecv
			}
			if c.closed != 0 {
				goto rclose
			}

		// 2: chan <- obj 的 select case 语句块
		case caseSend:
			if raceenabled {
				racereadpc(c.raceaddr(), cas.pc, chansendpc)
			}
			if c.closed != 0 {
				goto sclose
			}
			sg = c.recvq.dequeue()
			if sg != nil {
				goto send
			}
			if c.qcount < c.dataqsiz {
				goto bufsend
			}

		// 3: default 的 select case 语句块
		case caseDefault:
			dfli = casi
			dfl = cas
		}
	}

	if dfl != nil {
		selunlock(scases, lockorder) // select 逐个 解锁  chan
		casi = dfli
		cas = dfl
		goto retc
	}

	// pass 2 - enqueue on all chans
	gp = getg()
	if gp.waiting != nil {
		throw("gp.waiting != nil")
	}
	nextp = &gp.waiting
	for _, casei := range lockorder {
		casi = int(casei)
		cas = &scases[casi]
		if cas.kind == caseNil {
			continue
		}
		c = cas.c
		sg := acquireSudog()
		sg.g = gp
		sg.isSelect = true
		// No stack splits between assigning elem and enqueuing
		// sg on gp.waiting where copystack can find it.
		sg.elem = cas.elem
		sg.releasetime = 0
		if t0 != 0 {
			sg.releasetime = -1
		}
		sg.c = c
		// Construct waiting list in lock order.
		*nextp = sg
		nextp = &sg.waitlink

		switch cas.kind {
		case caseRecv:
			c.recvq.enqueue(sg)

		case caseSend:
			c.sendq.enqueue(sg)
		}
	}

	// wait for someone to wake us up
	gp.param = nil
	gopark(selparkcommit, nil, waitReasonSelect, traceEvGoBlockSelect, 1)
	gp.activeStackChans = false

	sellock(scases, lockorder)  // 锁定 select 中涉及的所有 chan

	gp.selectDone = 0
	sg = (*sudog)(gp.param)
	gp.param = nil

	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	casi = -1
	cas = nil
	sglist = gp.waiting
	// Clear all elem before unlinking from gp.waiting.
	for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
		sg1.isSelect = false
		sg1.elem = nil
		sg1.c = nil
	}
	gp.waiting = nil

	for _, casei := range lockorder {
		k = &scases[casei]
		if k.kind == caseNil {
			continue
		}
		if sglist.releasetime > 0 {
			k.releasetime = sglist.releasetime
		}
		if sg == sglist {
			// sg has already been dequeued by the G that woke us up.
			casi = int(casei)
			cas = k
		} else {
			c = k.c
			if k.kind == caseSend {
				c.sendq.dequeueSudoG(sglist)
			} else {
				c.recvq.dequeueSudoG(sglist)
			}
		}
		sgnext = sglist.waitlink
		sglist.waitlink = nil
		releaseSudog(sglist)
		sglist = sgnext
	}

	if cas == nil {
		// We can wake up with gp.param == nil (so cas == nil)
		// when a channel involved in the select has been closed.
		// It is easiest to loop and re-run the operation;
		// we'll see that it's now closed.
		// Maybe some day we can signal the close explicitly,
		// but we'd have to distinguish close-on-reader from close-on-writer.
		// It's easiest not to duplicate the code and just recheck above.
		// We know that something closed, and things never un-close,
		// so we won't block again.
		goto loop
	}

	c = cas.c

	if debugSelect {
		print("wait-return: cas0=", cas0, " c=", c, " cas=", cas, " kind=", cas.kind, "\n")
	}

	if cas.kind == caseRecv {
		recvOK = true
	}

	if raceenabled {
		if cas.kind == caseRecv && cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, cas.pc, chanrecvpc)
		} else if cas.kind == caseSend {
			raceReadObjectPC(c.elemtype, cas.elem, cas.pc, chansendpc)
		}
	}
	if msanenabled {
		if cas.kind == caseRecv && cas.elem != nil {
			msanwrite(cas.elem, c.elemtype.size)
		} else if cas.kind == caseSend {
			msanread(cas.elem, c.elemtype.size)
		}
	}

	selunlock(scases, lockorder) // select 逐个 解锁  chan
	goto retc

bufrecv:
	// can receive from buffer
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, cas.pc, chanrecvpc)
		}
		raceacquire(chanbuf(c, c.recvx))
		racerelease(chanbuf(c, c.recvx))
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.size)
	}
	recvOK = true
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(scases, lockorder) // select 逐个 解锁  chan
	goto retc

bufsend:
	// can send to buffer
	if raceenabled {
		raceacquire(chanbuf(c, c.sendx))
		racerelease(chanbuf(c, c.sendx))
		raceReadObjectPC(c.elemtype, cas.elem, cas.pc, chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(scases, lockorder) // select 逐个 解锁  chan
	goto retc

recv:
	// can receive from sleeping sender (sg)
	recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2) // select 逐个 解锁  chan
	if debugSelect {
		print("syncrecv: cas0=", cas0, " c=", c, "\n")
	}
	recvOK = true
	goto retc

rclose:
	// read at end of closed channel
	selunlock(scases, lockorder) // select 逐个 解锁  chan
	recvOK = false
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	goto retc

send:
	// can send to a sleeping receiver (sg)
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, cas.pc, chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2) // select 逐个 解锁  chan
	if debugSelect {
		print("syncsend: cas0=", cas0, " c=", c, "\n")
	}
	goto retc

retc:
	if cas.releasetime > 0 {
		blockevent(cas.releasetime-t0, 1)
	}

	// 返回 select  case 的 index  和 是否可以接收 chan
	return casi, recvOK

sclose:
	// send on closed channel
	selunlock(scases, lockorder)  // select 逐个 解锁  chan
	panic(plainError("send on closed channel"))
}

func (c *hchan) sortkey() uintptr {
	return uintptr(unsafe.Pointer(c))
}

/**
// runtimeSelect 是传递给rselect的单个案例。
// 必须匹配../reflect/value.go:/runtimeSelect
 */
// A runtimeSelect is a single case passed to rselect.
// This must match ../reflect/value.go:/runtimeSelect
type runtimeSelect struct {

	// select case 的 方向
	dir selectDir
	typ unsafe.Pointer // channel type (not used here)

	// 这个是 当前 select 的 case 中的 chan 引用
	ch  *hchan         // channel

	val unsafe.Pointer // ptr to data (SendDir) or ptr to receive buffer (RecvDir)
}

// These values must match ../reflect/value.go:/SelectDir.
type selectDir int

const (
	_             selectDir = iota
	selectSend              // case Chan <- Send
	selectRecv              // case <-Chan:
	selectDefault           // default
)

// select 关键字的执行对外的 入口
//
// 实现了 value.go 的 reflect.rselect()
//
//go:linkname reflect_rselect reflect.rselect
func reflect_rselect(cases []runtimeSelect) (int, bool) {
	if len(cases) == 0 {
		block()
	}

	// 旧版本中还有 `hselect` 结构 和 `scase` 结构
	//	然后 sel 使用了  malloc 函数去开启 hselect 结构的内存
	//	最后通过 selectsend()、selectrecv()、selectdefault() 分别将 代表 select 的 case 和default 的 `scase` 注册到 `hselect`
	//
	//	而 新的实现已经移除了 `hselect` 结构, 只保留了 `scase` 结构.
	// 	在注册 select case 语句块时, 直接通过下面的 `sel := make([]scase, len(cases))`
	// 	然后逐个 将 `scase` 加到里头
	sel := make([]scase, len(cases))
	order := make([]uint16, 2*len(cases))	// order0为一个两倍cas0数组长度的buffer， 保存scase随机序列 pollorder 和 scase中channel地址序列 lockorder, todo 看我的博客知道旧的实现中 pollorder 和 lockorder 分别是啥了

	// 遍历 所有 case及default 语句实例  todo 根据 []runtimeSelect 切片 构造 []scase 切片
	for i := range cases {

		// 取出 每一个 case 和 default 语句块
		rc := &cases[i]

		// 判断 case 中 chan 的方向
		switch rc.dir {

		// default 语句块
		case selectDefault:
			sel[i] = scase{kind: caseDefault}

		// case chan <- obj 发送通道语句块
		case selectSend:
			sel[i] = scase{kind: caseSend, c: rc.ch, elem: rc.val}

		// case obj <- chan 接收通道语句块
		case selectRecv:
			sel[i] = scase{kind: caseRecv, c: rc.ch, elem: rc.val}
		}

		if raceenabled || msanenabled {
			selectsetpc(&sel[i])
		}
	}


	return selectgo(&sel[0], &order[0], len(cases))  // todo 真正 去执行 select 语句
}

func (q *waitq) dequeueSudoG(sgp *sudog) {
	x := sgp.prev
	y := sgp.next
	if x != nil {
		if y != nil {
			// middle of queue
			x.next = y
			y.prev = x
			sgp.next = nil
			sgp.prev = nil
			return
		}
		// end of queue
		x.next = nil
		q.last = x
		sgp.prev = nil
		return
	}
	if y != nil {
		// start of queue
		y.prev = nil
		q.first = y
		sgp.next = nil
		return
	}

	// x==y==nil. Either sgp is the only element in the queue,
	// or it has already been removed. Use q.first to disambiguate.
	if q.first == sgp {
		q.first = nil
		q.last = nil
	}
}
