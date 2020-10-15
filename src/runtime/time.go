// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Time-related runtime and pieces of package time.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// todo runtimeTimer的实现  (Timer 和 Ticker 中都是用了这个东西)
//
//     todo 这个 结构体的 类型, 必须要和  time/sleep.go 中的 runtimeTimer 结构体结构一致
// Package time knows the layout of this structure.
// If this struct changes, adjust ../time/sleep.go:/runtimeTimer.
type timer struct {
	// If this timer is on a heap, which P's heap it is on.
	// puintptr rather than *p to match uintptr in the versions
	// of this struct defined in other packages.
	//
	//	如果此计时器在堆上，那么它在哪个P的堆上
	//	在其他包中定义的此结构的版本中，使用 puintptr而不是 *p 来匹配 uintptr
	pp puintptr   // 被关联该 timer 的 P的地址

	// Timer wakes up at when, and then at when+period, ... (period > 0 only)
	// each time calling f(arg, now) in the timer goroutine, so f must be
	// a well-behaved function and not block.
	when   int64			// 定时时间点
	period int64			// 当前定时器周期触发间隔 (Ticker 中使用到) （对于Timer来说， 此值恒为0）
	f      func(interface{}, uintptr)		// 定时器触发时执行函数 f
	arg    interface{}						// 定时器触发时执行函数 f 的第一个参数
	seq    uintptr							// 定时器触发时执行函数 f 的第二个参数  (该参数只在网络收发场景下使用)  （Timer并不使用该参数）  todo 目前这个参数 Timer 和 Ticker 都没用到

	// What to set the when field to in timerModifiedXX status.  将when字段设置为timerModifiedXX状态的内容
	//
	// 用来记录 变更的 新定时时间点
	nextwhen int64

	// The status field holds one of the values below.
	//
	// timer 的状态
	status uint32
}

// Code outside this file has to be careful in using a timer value.
//
// The pp, status, and nextwhen fields may only be used by code in this file.
//
// Code that creates a new timer value can set the when, period, f,
// arg, and seq fields.
// A new timer value may be passed to addtimer (called by time.startTimer).
// After doing that no fields may be touched.
//
// An active timer (one that has been passed to addtimer) may be
// passed to deltimer (time.stopTimer), after which it is no longer an
// active timer. It is an inactive timer.
// In an inactive timer the period, f, arg, and seq fields may be modified,
// but not the when field.
// It's OK to just drop an inactive timer and let the GC collect it.
// It's not OK to pass an inactive timer to addtimer.
// Only newly allocated timer values may be passed to addtimer.
//
// An active timer may be passed to modtimer. No fields may be touched.
// It remains an active timer.
//
// An inactive timer may be passed to resettimer to turn into an
// active timer with an updated when field.
// It's OK to pass a newly allocated timer value to resettimer.
//
// Timer operations are addtimer, deltimer, modtimer, resettimer,
// cleantimers, adjusttimers, and runtimer.
//
// We don't permit calling addtimer/deltimer/modtimer/resettimer simultaneously,
// but adjusttimers and runtimer can be called at the same time as any of those.
//
// Active timers live in heaps attached to P, in the timers field.
// Inactive timers live there too temporarily, until they are removed.
//
// addtimer:
//   timerNoStatus   -> timerWaiting
//   anything else   -> panic: invalid value
// deltimer:
//   timerWaiting         -> timerModifying -> timerDeleted
//   timerModifiedEarlier -> timerModifying -> timerDeleted
//   timerModifiedLater   -> timerModifying -> timerDeleted
//   timerNoStatus        -> do nothing
//   timerDeleted         -> do nothing
//   timerRemoving        -> do nothing
//   timerRemoved         -> do nothing
//   timerRunning         -> wait until status changes
//   timerMoving          -> wait until status changes
//   timerModifying       -> wait until status changes
// modtimer:
//   timerWaiting    -> timerModifying -> timerModifiedXX
//   timerModifiedXX -> timerModifying -> timerModifiedYY
//   timerNoStatus   -> timerModifying -> timerWaiting
//   timerRemoved    -> timerModifying -> timerWaiting
//   timerDeleted    -> timerModifying -> timerModifiedXX
//   timerRunning    -> wait until status changes
//   timerMoving     -> wait until status changes
//   timerRemoving   -> wait until status changes
//   timerModifying  -> wait until status changes
// cleantimers (looks in P's timer heap):
//   timerDeleted    -> timerRemoving -> timerRemoved
//   timerModifiedXX -> timerMoving -> timerWaiting
// adjusttimers (looks in P's timer heap):
//   timerDeleted    -> timerRemoving -> timerRemoved
//   timerModifiedXX -> timerMoving -> timerWaiting
// runtimer (looks in P's timer heap):
//   timerNoStatus   -> panic: uninitialized timer
//   timerWaiting    -> timerWaiting or
//   timerWaiting    -> timerRunning -> timerNoStatus or
//   timerWaiting    -> timerRunning -> timerWaiting
//   timerModifying  -> wait until status changes
//   timerModifiedXX -> timerMoving -> timerWaiting
//   timerDeleted    -> timerRemoving -> timerRemoved
//   timerRunning    -> panic: concurrent runtimer calls
//   timerRemoved    -> panic: inconsistent timer heap
//   timerRemoving   -> panic: inconsistent timer heap
//   timerMoving     -> panic: inconsistent timer heap

// Values for the timer status field.
const (
	// Timer has no status set yet.
	timerNoStatus = iota    											//  定时器 还未设置 状态

	// Waiting for timer to fire.
	// The timer is in some P's heap.
	timerWaiting

	// Running the timer function.
	// A timer will only have this status briefly.
	timerRunning

	// The timer is deleted and should be removed.
	// It should not be run, but it is still in some P's heap.
	timerDeleted

	// The timer is being removed.
	// The timer will only have this status briefly.
	timerRemoving

	// The timer has been stopped.
	// It is not in any P's heap.
	timerRemoved

	// The timer is being modified.
	// The timer will only have this status briefly.
	timerModifying

	// The timer has been modified to an earlier time.
	// The new when value is in the nextwhen field.
	// The timer is in some P's heap, possibly in the wrong place.
	timerModifiedEarlier

	// The timer has been modified to the same or a later time.
	// The new when value is in the nextwhen field.
	// The timer is in some P's heap, possibly in the wrong place.
	timerModifiedLater

	// The timer has been modified and is being moved.
	// The timer will only have this status briefly.
	timerMoving
)

// maxWhen is the maximum value for timer's when field.
const maxWhen = 1<<63 - 1

// verifyTimers can be set to true to add debugging checks that the
// timer heaps are valid.
const verifyTimers = false

// Package time APIs.
// Godoc uses the comments in package time, not these.

// time.now is implemented in assembly.


// 让当前 G 休眠 ns 纳秒
// timeSleep puts the current goroutine to sleep for at least ns nanoseconds.
//go:linkname timeSleep time.Sleep
func timeSleep(ns int64) {		// time.Sleep() 的真正实现
	if ns <= 0 {
		return
	}

	// 获取 当前 G
	gp := getg()

	// 获取 G 上的 timer
	t := gp.timer

	// 一般来说, 最开始  timer 肯定是 nil
	if t == nil {
		t = new(timer)
		gp.timer = t  // 实力一个新的 timer 给 G
	}

	// 设置 这个 timer 的 定时 回调函数
	t.f = goroutineReady   	// 就是简单的 唤醒当前 G
	t.arg = gp				// 回调 func 的第一参数就是 当前 G
	t.nextwhen = nanotime() + ns	// 设置 定时时间      todo time.Sleep() 是将时间 设置到  nextwhen字段的.  不是 when 字段

	// 将当前 G 进行休眠
	//
	//    todo 最终由  resetForSleep() 取 上面设置的  nextwhen 去赋值给 when， 然后 定时到 when 去调用回调函数  goroutineReady()  最终调用 goready() 重新唤醒 G
	gopark(resetForSleep, unsafe.Pointer(t), waitReasonSleep, traceEvGoSleep, 1)
}

// resetForSleep is called after the goroutine is parked for timeSleep.
// We can't call resettimer in timeSleep itself because if this is a short
// sleep and there are many goroutines then the P can wind up running the
// timer function, goroutineReady, before the goroutine has been parked.
//
//
//	`resetForSleep()` 将休眠在timeSleep之后的 goroutine  唤醒
//						我们无法在timeSleep本身中调用resettimer，因为如果这是短暂的睡眠，并且有许多goroutine，则P可以在暂存goroutine之前结束运行定时器功能 `goroutineReady()`
func resetForSleep(gp *g, ut unsafe.Pointer) bool {
	t := (*timer)(ut)
	resettimer(t, t.nextwhen)   // 重新被 唤醒 G 时 (time.Sleep()后被唤醒) 将之前的 nextwhen 传进去
	return true
}

// startTimer adds t to the timer heap.
//go:linkname startTimer time.startTimer
func startTimer(t *timer) {		// 实现了 time.startTimer()
	if raceenabled {
		racerelease(unsafe.Pointer(t))
	}

	// 将 timer 追加到 timer队列 中
	addtimer(t)
}

// stopTimer stops a timer.
// It reports whether t was stopped before being run.
//go:linkname stopTimer time.stopTimer
func stopTimer(t *timer) bool {			// 实现了 time.stopTimer()
	return deltimer(t)
}

// resetTimer resets an inactive timer, adding it to the heap.
//go:linkname resetTimer time.resetTimer
func resetTimer(t *timer, when int64) {		// 实现了 time.resetTimer()
	if raceenabled {
		racerelease(unsafe.Pointer(t))
	}
	resettimer(t, when)
}

// Go runtime.

// todo 唤醒当前 G
// Ready the goroutine arg.
func goroutineReady(arg interface{}, seq uintptr) {
	goready(arg.(*g), 0)
}

// addtimer adds a timer to the current P.
// This should only be called with a newly created timer.
// That avoids the risk of changing the when field of a timer in some P's heap,
// which could cause the heap to become unsorted.
//
//	todo 添加 timer 到 P 的 timer堆中
//
// `addtimer()` 将计时器添加到当前P
//				仅应使用 新创建的 计时器 来调用它
//				这样可以避免更改某些P堆中的计时器的when字段的风险，因为这可能导致堆变为未排序状态
func addtimer(t *timer) {
	// when must never be negative; otherwise runtimer will overflow
	// during its delta calculation and never expire other runtime timers.
	//
	//
	// 永远不能为负;  否则 runtimer 将在 增量计算期间溢出, 并且永不终止其他运行时计时器
	if t.when < 0 {
		t.when = maxWhen
	}
	if t.status != timerNoStatus {     // 最开始的timer 肯定是 无状态的 timer
		throw("addtimer called with initialized timer")
	}
	t.status = timerWaiting    	// 初识状态 设为  "等待中"

	when := t.when				// 取出 定时的时间点

	pp := getg().m.p.ptr()		// 获取当前 G 中的 P
	lock(&pp.timersLock)
	cleantimers(pp)				// 清除 P 的 timer 堆汇总 旧有的 第一个 timer  todo (当然, 这个方法里面可能 啥事没做,  看实现自明)   看下面 deltimer() 就知道 这里为什么需要做一次 清除了
	doaddtimer(pp, t)			// 将当前 timer 追加到 P 的 timer 堆中
	unlock(&pp.timersLock)

	// 唤醒 【网络轮询器】
	wakeNetPoller(when)  // 里头很多事 汇编实现的
}

// doaddtimer adds t to the current P's heap.
// The caller must have locked the timers for pp.
//
//
// `doaddtimer()` 将t添加到当前P的堆中
//     			调用者必须锁定pp的计时器。
func doaddtimer(pp *p, t *timer) {
	// Timers rely on the network poller, so make sure the poller
	// has started.
	//
	//
	// 计时器依赖于网络轮询器，因此请确保轮询器已启动
	if netpollInited == 0 {
		netpollGenericInit()
	}


	// 最开始的 timer 是还没被关联到 某个 P上的, 所以 timer中 P 的地址应该为 0
	if t.pp != 0 {
		throw("doaddtimer: P already set in timer")
	}

	// 给timer 设置 关联当前 timer 的 P 的地址
	t.pp.set(pp)

	// 取出 该 P 中的 timer 堆的长度
	i := len(pp.timers)

	// 将 timer 追加到 堆末尾
	pp.timers = append(pp.timers, t)

	// 调整堆
	siftupTimer(pp.timers, i)

	// 取出 新的 堆头的 timer 的 when, 更新 P 的字段
	if t == pp.timers[0] {
		atomic.Store64(&pp.timer0When, uint64(t.when))
	}

	// 更新 P 的字段  timer 数目 +1
	atomic.Xadd(&pp.numTimers, 1)
}

// deltimer deletes the timer t. It may be on some other P, so we can't
// actually remove it from the timers heap. We can only mark it as deleted.
// It will be removed in due course by the P whose heap it is on.
// Reports whether the timer was removed before it was run.
//
// todo 清除 timer
//
//	`deltimer（）`删除计时器t
// 				它可能在其他P上，因此我们实际上无法从 计时器堆中将其删除. 我们只能将其标记为已删除.   todo 所以才有了在 addtimer() 中先调用一次  cleantimers() 先看看第一个 timer是否可以被删除掉
//				它会在适当的时候被其堆所在的P删除
//				报告计时器在运行之前是否已删除
func deltimer(t *timer) bool {
	for {

		// 还是先看状态
		switch s := atomic.Load(&t.status); s {

		// 【在等待】 || 【修改为更晚】
		case timerWaiting, timerModifiedLater:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			//
			// 禁止 当前 G 被 抢占
			//
			//	在计时器处于timerModifying时防止抢占   todo (因为可能在删除前, 做了一次 修改时间的动作, 所以可能此时正在做 修改定时时间的过程中...)
			//	这可能导致自锁。 参见＃38070。
			mp := acquirem()
			if atomic.Cas(&t.status, s, timerModifying) {   // 先将状态改为  【修改中】
				// Must fetch t.pp before changing status,
				// as cleantimers in another goroutine
				// can clear t.pp of a timerDeleted timer.
				//
				// 必须在更改状态之前获取 t.pp,  todo 因为另一个goroutine中的cleantimers可以清除timer.deleted计时器的t.pp
				tpp := t.pp.ptr()
				if !atomic.Cas(&t.status, timerModifying, timerDeleted) {  // 马上修改状态为  【可被删除】
					badTimer()
				}
				releasem(mp)  // 释放当前 G 被抢占
				atomic.Xadd(&tpp.deletedTimers, 1)			// 增加当前 P 的 【可被删除】 timer 的个数计数
				// Timer was not yet run.
				return true
			} else {

				// 如果操作不成功, 直接 释放当前 G 被抢占
				releasem(mp)
			}

		// 【修改为更早】
		case timerModifiedEarlier:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			//
			// 禁止 当前 G 被 抢占
			//
			//	在计时器处于timerModifying时防止抢占   todo (因为可能在删除前, 做了一次 修改时间的动作, 所以可能此时正在做 修改定时时间的过程中...)
			//	这可能导致自锁。 参见＃38070。
			mp := acquirem()
			if atomic.Cas(&t.status, s, timerModifying) {  // 先将状态改为  【修改中】
				// Must fetch t.pp before setting status
				// to timerDeleted.
				//
				// 在将状态设置为 【可悲删除】 之前必须获取t.pp
				tpp := t.pp.ptr()
				atomic.Xadd(&tpp.adjustTimers, -1)
				if !atomic.Cas(&t.status, timerModifying, timerDeleted) {
					badTimer()
				}
				releasem(mp)
				atomic.Xadd(&tpp.deletedTimers, 1)
				// Timer was not yet run.
				return true
			} else {
				releasem(mp)
			}

		// 【可被删除】 || 【在删除中】 || 【已经被删除】
		case timerDeleted, timerRemoving, timerRemoved:
			// Timer was already run.
			return false  // 直接返回

		// 【正在运行】 || 【正在被调整移动位置】
		case timerRunning, timerMoving:
			// The timer is being run or moved, by a different P.
			// Wait for it to complete.
			//
			//	计时器正在由另一个P运行 或 移动
			//	等待它完成
			osyield()
		case timerNoStatus:
			// Removing timer that was never added or
			// has already been run. Also see issue 21874.
			//
			// 删除 从未添加 或 已运行 的计时器。 另请参阅问题21874。
			return false

		// 【正在被修改中】
		case timerModifying:
			// Simultaneous calls to deltimer and modtimer.
			// Wait for the other call to complete.
			//
			//	可能是同时 被调用 deltimer() 和 modtimer()
			//	等待另一个调用完成
			osyield()
		default:
			badTimer()
		}
	}
}

// dodeltimer removes timer i from the current P's heap.
// We are locked on the P when this is called.
// It reports whether it saw no problems due to races.
// The caller must have locked the timers for pp.
func dodeltimer(pp *p, i int) {
	if t := pp.timers[i]; t.pp.ptr() != pp {
		throw("dodeltimer: wrong P")
	} else {
		t.pp = 0
	}
	last := len(pp.timers) - 1
	if i != last {
		pp.timers[i] = pp.timers[last]
	}
	pp.timers[last] = nil
	pp.timers = pp.timers[:last]
	if i != last {
		// Moving to i may have moved the last timer to a new parent,
		// so sift up to preserve the heap guarantee.
		siftupTimer(pp.timers, i)
		siftdownTimer(pp.timers, i)
	}
	if i == 0 {
		updateTimer0When(pp)
	}
	atomic.Xadd(&pp.numTimers, -1)
}

// dodeltimer0 removes timer 0 from the current P's heap.
// We are locked on the P when this is called.
// It reports whether it saw no problems due to races.
// The caller must have locked the timers for pp.
func dodeltimer0(pp *p) {

	// 检查下 timer 中的 P 是不是 当前操作 timer 的 P
	if t := pp.timers[0]; t.pp.ptr() != pp {
		throw("dodeltimer0: wrong P")
	} else {
		t.pp = 0  // 清空当前 timer 中的 P 地址
	}
	last := len(pp.timers) - 1   // 堆长度 -1
	if last > 0 {
		pp.timers[0] = pp.timers[last]  // 将最后一个 提到 堆首
	}
	pp.timers[last] = nil   // 删除 最后一个元素
	pp.timers = pp.timers[:last]  // 更新 堆 数组  (已经是 删除掉 第一个元素的 堆数组 了)

	// 调整堆
	if last > 0 {
		siftdownTimer(pp.timers, 0)
	}
	updateTimer0When(pp)   // 更新 P 上的 第一个 timer 的 定时时间点
	atomic.Xadd(&pp.numTimers, -1)  // 减少 P 上的 timer 个数计数
}

// todo 修改 timer 的定时时间点
// modtimer modifies an existing timer.
// This is called by the netpoll code.
func modtimer(t *timer, when, period int64, f func(interface{}, uintptr), arg interface{}, seq uintptr) {

	// when 为修改 定时器的 新时间值
	if when < 0 {
		when = maxWhen
	}

	status := uint32(timerNoStatus)		// 是否是 无状态的timer
	wasRemoved := false					// 是否已经被 清除
	var mp *m
loop:
	for {


		switch status = atomic.Load(&t.status); status {

		// 【在等待中】 || 【修改为更早】 || 【修改为更晚】
		case timerWaiting, timerModifiedEarlier, timerModifiedLater:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			//
			// 禁止 当前 G 被 抢占
			//
			//	在计时器处于timerModifying时防止抢占   todo (因为可能在删除前, 做了一次 修改时间的动作, 所以可能此时正在做 修改定时时间的过程中...)
			//	这可能导致自锁。 参见＃38070。
			mp = acquirem()
			if atomic.Cas(&t.status, status, timerModifying) { // 先将状态改为  【修改中】
				break loop
			}

			releasem(mp) // 释放 当前 G 被抢占

		// 【无状态】 || 【已经被删除】
		case timerNoStatus, timerRemoved:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			//
			// 禁止 当前 G 被 抢占
			//
			//	在计时器处于timerModifying时防止抢占   todo (因为可能在删除前, 做了一次 修改时间的动作, 所以可能此时正在做 修改定时时间的过程中...)
			//	这可能导致自锁。 参见＃38070。
			mp = acquirem()

			// Timer was already run and t is no longer in a heap.
			// Act like addtimer.
			//
			// Timer已经运行，并且t不再在堆中。
			// 就像新调用  addtimer() 一样
			if atomic.Cas(&t.status, status, timerModifying) {    // 先将状态改为  【修改中】
				wasRemoved = true  	// 认为已经被删除
				break loop			// 跳出 for
			}
			releasem(mp)  // 释放 当前 G 被抢占


		// 【可以被删除】
		case timerDeleted:
			// Prevent preemption while the timer is in timerModifying.
			// This could lead to a self-deadlock. See #38070.
			//
			// 禁止 当前 G 被 抢占
			//
			//	在计时器处于timerModifying时防止抢占   todo (因为可能在删除前, 做了一次 修改时间的动作, 所以可能此时正在做 修改定时时间的过程中...)
			//	这可能导致自锁。 参见＃38070。
			mp = acquirem()
			if atomic.Cas(&t.status, status, timerModifying) {  // 先将状态改为  【修改中】
				atomic.Xadd(&t.pp.ptr().deletedTimers, -1)		// 将对应的 P 中的 【可被删除】 状态的 timer 计数 -1
				break loop					// 跳出 for
			}
			releasem(mp) // 释放 当前 G 被抢占


		// 【正在运行】 || 【正在删除中】 || 【正在调整堆移动中】
		case timerRunning, timerRemoving, timerMoving:
			// The timer is being run or moved, by a different P.
			// Wait for it to complete.
			//
			//	计时器正在由另一个P 运行 或 移动
			//	等待它完成
			osyield()

		// 【 修改中】
		case timerModifying:
			// Multiple simultaneous calls to modtimer.
			// Wait for the other call to complete.
			//
			//	多次同时调用 modtimer()
			//	等待另一个调用完成
			osyield()

		// 肯定处于某种状态的, 所以 ...
		default:
			badTimer()
		}
	}


	// todo 重新赋值
	t.period = period
	t.f = f
	t.arg = arg
	t.seq = seq


	// 只有 经历过 【无状态】 || 【已经被删除】 才能进到这里
	if wasRemoved {

		// todo 重新设置  定时时间点
		t.when = when

		// 获取当前 G 的 P
		pp := getg().m.p.ptr()
		lock(&pp.timersLock)
		doaddtimer(pp, t) 	// 将 timer 追加到 这个 P 的 timer 堆中
		unlock(&pp.timersLock)
		if !atomic.Cas(&t.status, timerModifying, timerWaiting) { // 修改状态为 【等待中】
			badTimer()
		}
		releasem(mp)
		wakeNetPoller(when)	// todo 唤醒 网络轮询器 执行定时器


	// 否则, 在其他状态下的 重置定时时间时.    todo 一般是 因为 timer 在 其他 P 中, 我们不能直接操作 timer 只能是 修改了 timer 的 状态值, 由 当前 P 自己去操作 Timer
	} else {


		// The timer is in some other P's heap, so we can't change
		// the when field. If we did, the other P's heap would
		// be out of order. So we put the new when value in the
		// nextwhen field, and let the other P set the when field
		// when it is prepared to resort the heap.
		//
		// todo 这或许就是 t的 nextwhen 字段存在的意义
		//
		// 计时器位于 其他 P 的堆中，因此我们无法更改when字段.
		// 如果这样做，其他P的堆将混乱.
		// 因此，我们将新的when值放在nextwhen字段中，让另一个P在准备使用堆时设置when字段
		t.nextwhen = when    // todo 这或许就是 t的 nextwhen 字段存在的意义

		// todo 设置 修改时间状态为 【更早】 或者 【更晚】
		newStatus := uint32(timerModifiedLater)		// 只有在这里一个地方 赋值为 【过晚】
		if when < t.when {
			newStatus = timerModifiedEarlier    	// 只有在这里一个地方 赋值为 【过早】
		}


		// Update the adjustTimers field.  Subtract one if we
		// are removing a timerModifiedEarlier, add one if we
		// are adding a timerModifiedEarlier.
		//
		//
		// 更新 P 的 adjustTimers字段。
		// 如果要删除 timerModifiedEarlier，则 减1;
		// 如果要添加 timerModifiedEarlier，则 加1.
		adjust := int32(0)
		if status == timerModifiedEarlier {
			adjust--
		}
		if newStatus == timerModifiedEarlier {
			adjust++
		}
		if adjust != 0 {
			atomic.Xadd(&t.pp.ptr().adjustTimers, adjust)
		}

		// Set the new status of the timer.
		if !atomic.Cas(&t.status, timerModifying, newStatus) {
			badTimer()
		}
		releasem(mp)

		// If the new status is earlier, wake up the poller.
		if newStatus == timerModifiedEarlier {
			wakeNetPoller(when)  // 唤醒 网络轮询器
		}
	}
}


// todo 重置timer时间
//
//      when 为 新设置的 时间
//
// resettimer resets the time when a timer should fire.
// If used for an inactive timer, the timer will become active.
// This should be called instead of addtimer if the timer value has been,
// or may have been, used previously.
func resettimer(t *timer, when int64) {
	modtimer(t, when, t.period, t.f, t.arg, t.seq)
}

// cleantimers cleans up the head of the timer queue. This speeds up
// programs that create and delete timers; leaving them in the heap
// slows down addtimer. Reports whether no timer problems were found.
// The caller must have locked the timers for pp.
//
//
// `cleantimers()`   清理计时器队列的 头部timer
// 					这样可以加快 创建 和 删除 计时器的程序的速度
// 					将它们留在堆中会减慢 `addtimer()` 函数的速度
// 					报告是否未发现计时器问题
//	 调用者 必须锁定pp的计时器
func cleantimers(pp *p) {
	for {
		if len(pp.timers) == 0 {
			return
		}

		// 先拿出 P 中 timer 队列的 第一个 timer
		t := pp.timers[0]

		// 对比下 timer 中的 P 和当前操作 timer 的 P 是不是同一个
		if t.pp.ptr() != pp {
			throw("cleantimers: bad p")
		}

		// 获取 timer 的状态
		switch s := atomic.Load(&t.status); s {

		// 【可被删除】
		case timerDeleted:
			if !atomic.Cas(&t.status, s, timerRemoving) {  // 变更 状态为 删除中
				continue  // 更改失败, 重新尝试
			}
			dodeltimer0(pp)  // 删除掉 堆 中的 第一个  timer
			if !atomic.Cas(&t.status, timerRemoving, timerRemoved) {  // 变更 状态为 已被删除
				badTimer()
			}
			atomic.Xadd(&pp.deletedTimers, -1)		// P 的 timer堆中 状态位 【可被删除】 的timer 的个数 -1

		// 【变更定时时间】
		case timerModifiedEarlier, timerModifiedLater:
			if !atomic.Cas(&t.status, s, timerMoving) {   // 变更 状态为 移动中 (在堆中 调整到新位置的移动)
				continue // 更改失败, 重新尝试
			}

			// Now we can change the when field.
			t.when = t.nextwhen    // 将新变更的 定时时间点 赋值给 when字段
			// Move t to the right position.
			dodeltimer0(pp)   // 删掉 P 中timer堆的 首timer , 也就是当前在操作的 timer
			doaddtimer(pp, t) // 将 当前 timer 重新加入 堆  (when 被更新了)
			if s == timerModifiedEarlier {							// 如果 变更为 更早, 则 ...
				atomic.Xadd(&pp.adjustTimers, -1)
			}


			if !atomic.Cas(&t.status, timerMoving, timerWaiting) {  // 更改时间完成后, 将该timer 的状态置为 【等待中】
				badTimer()		// 修改失败,  抛异常
			}
		default:
			// Head of timers does not need adjustment.
			return
		}
	}
}

// moveTimers moves a slice of timers to pp. The slice has been taken
// from a different P.
// This is currently called when the world is stopped, but the caller
// is expected to have locked the timers for pp.
func moveTimers(pp *p, timers []*timer) {
	for _, t := range timers {
	loop:
		for {
			switch s := atomic.Load(&t.status); s {
			case timerWaiting:
				t.pp = 0
				doaddtimer(pp, t)
				break loop
			case timerModifiedEarlier, timerModifiedLater:
				if !atomic.Cas(&t.status, s, timerMoving) {
					continue
				}
				t.when = t.nextwhen
				t.pp = 0
				doaddtimer(pp, t)
				if !atomic.Cas(&t.status, timerMoving, timerWaiting) {
					badTimer()
				}
				break loop
			case timerDeleted:
				if !atomic.Cas(&t.status, s, timerRemoved) {
					continue
				}
				t.pp = 0
				// We no longer need this timer in the heap.
				break loop
			case timerModifying:
				// Loop until the modification is complete.
				osyield()
			case timerNoStatus, timerRemoved:
				// We should not see these status values in a timers heap.
				badTimer()
			case timerRunning, timerRemoving, timerMoving:
				// Some other P thinks it owns this timer,
				// which should not happen.
				badTimer()
			default:
				badTimer()
			}
		}
	}
}

// adjusttimers looks through the timers in the current P's heap for
// any timers that have been modified to run earlier, and puts them in
// the correct place in the heap. While looking for those timers,
// it also moves timers that have been modified to run later,
// and removes deleted timers. The caller must have locked the timers for pp.
func adjusttimers(pp *p) {
	if len(pp.timers) == 0 {
		return
	}
	if atomic.Load(&pp.adjustTimers) == 0 {
		if verifyTimers {
			verifyTimerHeap(pp)
		}
		return
	}
	var moved []*timer
loop:
	for i := 0; i < len(pp.timers); i++ {
		t := pp.timers[i]
		if t.pp.ptr() != pp {
			throw("adjusttimers: bad p")
		}
		switch s := atomic.Load(&t.status); s {
		case timerDeleted:
			if atomic.Cas(&t.status, s, timerRemoving) {
				dodeltimer(pp, i)
				if !atomic.Cas(&t.status, timerRemoving, timerRemoved) {
					badTimer()
				}
				atomic.Xadd(&pp.deletedTimers, -1)
				// Look at this heap position again.
				i--
			}
		case timerModifiedEarlier, timerModifiedLater:
			if atomic.Cas(&t.status, s, timerMoving) {
				// Now we can change the when field.
				t.when = t.nextwhen
				// Take t off the heap, and hold onto it.
				// We don't add it back yet because the
				// heap manipulation could cause our
				// loop to skip some other timer.
				dodeltimer(pp, i)
				moved = append(moved, t)
				if s == timerModifiedEarlier {
					if n := atomic.Xadd(&pp.adjustTimers, -1); int32(n) <= 0 {
						break loop
					}
				}
				// Look at this heap position again.
				i--
			}
		case timerNoStatus, timerRunning, timerRemoving, timerRemoved, timerMoving:
			badTimer()
		case timerWaiting:
			// OK, nothing to do.
		case timerModifying:
			// Check again after modification is complete.
			osyield()
			i--
		default:
			badTimer()
		}
	}

	if len(moved) > 0 {
		addAdjustedTimers(pp, moved)
	}

	if verifyTimers {
		verifyTimerHeap(pp)
	}
}

// addAdjustedTimers adds any timers we adjusted in adjusttimers
// back to the timer heap.
func addAdjustedTimers(pp *p, moved []*timer) {
	for _, t := range moved {
		doaddtimer(pp, t)
		if !atomic.Cas(&t.status, timerMoving, timerWaiting) {
			badTimer()
		}
	}
}

// nobarrierWakeTime looks at P's timers and returns the time when we
// should wake up the netpoller. It returns 0 if there are no timers.
// This function is invoked when dropping a P, and must run without
// any write barriers. Therefore, if there are any timers that needs
// to be moved earlier, it conservatively returns the current time.
// The netpoller M will wake up and adjust timers before sleeping again.
//go:nowritebarrierrec
func nobarrierWakeTime(pp *p) int64 {
	if atomic.Load(&pp.adjustTimers) > 0 {
		return nanotime()
	} else {
		return int64(atomic.Load64(&pp.timer0When))
	}
}


// todo 尝试着 运行 Timer 堆中第一个 Timer
//
// runtimer examines the first timer in timers. If it is ready based on now,
// it runs the timer and removes or updates it.
// Returns 0 if it ran a timer, -1 if there are no more timers, or the time
// when the first timer should run.
// The caller must have locked the timers for pp.
// If a timer is run, this will temporarily unlock the timers.
//
//
// `runtimer()`  检查计时器中的第一个计时器
// 				如果基于现在已准备就绪，它将运行计时器并 删除 或 更新 它
//
//				如果运行计时器，则返回0；如果没有计时器，则返回-1；或者应该运行第一个计时器的时间
//
//				调用者必须锁定pp的计时器
//				如果运行计时器，则将暂时解锁计时器
//
//
//go:systemstack
func runtimer(pp *p, now int64) int64 {
	for {
		t := pp.timers[0]
		if t.pp.ptr() != pp {
			throw("runtimer: bad p")
		}
		switch s := atomic.Load(&t.status); s {
		case timerWaiting:
			if t.when > now {
				// Not ready to run.
				return t.when
			}

			if !atomic.Cas(&t.status, s, timerRunning) {
				continue
			}
			// Note that runOneTimer may temporarily unlock
			// pp.timersLock.   请注意，runOneTimer 可能会暂时解锁 pp.timersLock
			runOneTimer(pp, t, now)
			return 0

		case timerDeleted:
			if !atomic.Cas(&t.status, s, timerRemoving) {
				continue
			}
			dodeltimer0(pp)
			if !atomic.Cas(&t.status, timerRemoving, timerRemoved) {
				badTimer()
			}
			atomic.Xadd(&pp.deletedTimers, -1)
			if len(pp.timers) == 0 {
				return -1
			}

		case timerModifiedEarlier, timerModifiedLater:
			if !atomic.Cas(&t.status, s, timerMoving) {
				continue
			}
			t.when = t.nextwhen
			dodeltimer0(pp)
			doaddtimer(pp, t)
			if s == timerModifiedEarlier {
				atomic.Xadd(&pp.adjustTimers, -1)
			}
			if !atomic.Cas(&t.status, timerMoving, timerWaiting) {
				badTimer()
			}

		case timerModifying:
			// Wait for modification to complete.
			osyield()

		case timerNoStatus, timerRemoved:
			// Should not see a new or inactive timer on the heap.
			badTimer()
		case timerRunning, timerRemoving, timerMoving:
			// These should only be set when timers are locked,
			// and we didn't do it.
			badTimer()
		default:
			badTimer()
		}
	}
}

// runOneTimer runs a single timer.
// The caller must have locked the timers for pp.
// This will temporarily unlock the timers while running the timer function.
//
//
// `runOneTimer()` 运行一个计时器
//
//  调用者必须锁定pp的计时器
//  这将在运行时间功能时 临时 解锁 计时器
//
//
//go:systemstack
func runOneTimer(pp *p, t *timer, now int64) {
	if raceenabled {
		ppcur := getg().m.p.ptr()
		if ppcur.timerRaceCtx == 0 {
			ppcur.timerRaceCtx = racegostart(funcPC(runtimer) + sys.PCQuantum)
		}
		raceacquirectx(ppcur.timerRaceCtx, unsafe.Pointer(t))
	}

	f := t.f
	arg := t.arg
	seq := t.seq


	// todo timer 执行的是 Ticker 定时器
	if t.period > 0 {
		// Leave in heap but adjust next time to fire.  留在堆中，但下次调整触发时间。
		delta := t.when - now
		t.when += t.period * (1 + -delta/t.period)			// 更新下次 触发时间
		siftdownTimer(pp.timers, 0)						// 调整堆
		if !atomic.Cas(&t.status, timerRunning, timerWaiting) {
			badTimer()
		}

		// 设置P的 timer0When 字段
		updateTimer0When(pp)


	// todo timer 执行的是 Timer 定时器
	} else {
		// Remove from heap.
		dodeltimer0(pp) 		// 从堆中 移除 timer
		if !atomic.Cas(&t.status, timerRunning, timerNoStatus) {
			badTimer()
		}
	}

	if raceenabled {
		// Temporarily use the current P's racectx for g0.
		gp := getg()
		if gp.racectx != 0 {
			throw("runOneTimer: unexpected racectx")
		}
		gp.racectx = gp.m.p.ptr().timerRaceCtx
	}

	unlock(&pp.timersLock)


	// todo 执行 定时器 回调func
	f(arg, seq)

	lock(&pp.timersLock)

	if raceenabled {
		gp := getg()
		gp.racectx = 0
	}
}

// clearDeletedTimers removes all deleted timers from the P's timer heap.
// This is used to avoid clogging up the heap if the program
// starts a lot of long-running timers and then stops them.
// For example, this can happen via context.WithTimeout.
//
// This is the only function that walks through the entire timer heap,
// other than moveTimers which only runs when the world is stopped.
//
// The caller must have locked the timers for pp.
func clearDeletedTimers(pp *p) {
	cdel := int32(0)
	cearlier := int32(0)
	to := 0
	changedHeap := false
	timers := pp.timers
nextTimer:
	for _, t := range timers {
		for {
			switch s := atomic.Load(&t.status); s {
			case timerWaiting:
				if changedHeap {
					timers[to] = t
					siftupTimer(timers, to)
				}
				to++
				continue nextTimer
			case timerModifiedEarlier, timerModifiedLater:
				if atomic.Cas(&t.status, s, timerMoving) {
					t.when = t.nextwhen
					timers[to] = t
					siftupTimer(timers, to)
					to++
					changedHeap = true
					if !atomic.Cas(&t.status, timerMoving, timerWaiting) {
						badTimer()
					}
					if s == timerModifiedEarlier {
						cearlier++
					}
					continue nextTimer
				}
			case timerDeleted:
				if atomic.Cas(&t.status, s, timerRemoving) {
					t.pp = 0
					cdel++
					if !atomic.Cas(&t.status, timerRemoving, timerRemoved) {
						badTimer()
					}
					changedHeap = true
					continue nextTimer
				}
			case timerModifying:
				// Loop until modification complete.
				osyield()
			case timerNoStatus, timerRemoved:
				// We should not see these status values in a timer heap.
				badTimer()
			case timerRunning, timerRemoving, timerMoving:
				// Some other P thinks it owns this timer,
				// which should not happen.
				badTimer()
			default:
				badTimer()
			}
		}
	}

	// Set remaining slots in timers slice to nil,
	// so that the timer values can be garbage collected.
	for i := to; i < len(timers); i++ {
		timers[i] = nil
	}

	atomic.Xadd(&pp.deletedTimers, -cdel)
	atomic.Xadd(&pp.numTimers, -cdel)
	atomic.Xadd(&pp.adjustTimers, -cearlier)

	timers = timers[:to]
	pp.timers = timers
	updateTimer0When(pp)

	if verifyTimers {
		verifyTimerHeap(pp)
	}
}

// verifyTimerHeap verifies that the timer heap is in a valid state.
// This is only for debugging, and is only called if verifyTimers is true.
// The caller must have locked the timers.
func verifyTimerHeap(pp *p) {
	for i, t := range pp.timers {
		if i == 0 {
			// First timer has no parent.
			continue
		}

		// The heap is 4-ary. See siftupTimer and siftdownTimer.
		p := (i - 1) / 4
		if t.when < pp.timers[p].when {
			print("bad timer heap at ", i, ": ", p, ": ", pp.timers[p].when, ", ", i, ": ", t.when, "\n")
			throw("bad timer heap")
		}
	}
	if numTimers := int(atomic.Load(&pp.numTimers)); len(pp.timers) != numTimers {
		println("timer heap len", len(pp.timers), "!= numTimers", numTimers)
		throw("bad timer heap len")
	}
}

// updateTimer0When sets the P's timer0When field.
// The caller must have locked the timers for pp.
//
//
// `updateTimer0When()` 设置P的 timer0When 字段
//
//						调用者必须锁定pp的计时器
func updateTimer0When(pp *p) {
	if len(pp.timers) == 0 {
		atomic.Store64(&pp.timer0When, 0)
	} else {
		atomic.Store64(&pp.timer0When, uint64(pp.timers[0].when))
	}
}

// timeSleepUntil returns the time when the next timer should fire,
// and the P that holds the timer heap that that timer is on.
// This is only called by sysmon and checkdead.
func timeSleepUntil() (int64, *p) {
	next := int64(maxWhen)
	var pret *p

	// Prevent allp slice changes. This is like retake.
	lock(&allpLock)
	for _, pp := range allp {
		if pp == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			continue
		}

		c := atomic.Load(&pp.adjustTimers)
		if c == 0 {
			w := int64(atomic.Load64(&pp.timer0When))
			if w != 0 && w < next {
				next = w
				pret = pp
			}
			continue
		}

		lock(&pp.timersLock)
		for _, t := range pp.timers {
			switch s := atomic.Load(&t.status); s {
			case timerWaiting:
				if t.when < next {
					next = t.when
				}
			case timerModifiedEarlier, timerModifiedLater:
				if t.nextwhen < next {
					next = t.nextwhen
				}
				if s == timerModifiedEarlier {
					c--
				}
			}
			// The timers are sorted, so we only have to check
			// the first timer for each P, unless there are
			// some timerModifiedEarlier timers. The number
			// of timerModifiedEarlier timers is in the adjustTimers
			// field, used to initialize c, above.
			//
			// We don't worry about cases like timerModifying.
			// New timers can show up at any time,
			// so this function is necessarily imprecise.
			// Do a signed check here since we aren't
			// synchronizing the read of pp.adjustTimers
			// with the check of a timer status.
			if int32(c) <= 0 {
				break
			}
		}
		unlock(&pp.timersLock)
	}
	unlock(&allpLock)

	return next, pret
}

// Heap maintenance algorithms.
// These algorithms check for slice index errors manually.
// Slice index error can happen if the program is using racy
// access to timers. We don't want to panic here, because
// it will cause the program to crash with a mysterious
// "panic holding locks" message. Instead, we panic while not
// holding a lock.

func siftupTimer(t []*timer, i int) {
	if i >= len(t) {
		badTimer()
	}
	when := t[i].when
	tmp := t[i]
	for i > 0 {
		p := (i - 1) / 4 // parent     todo timer的堆实现： 最小四叉堆
		if when >= t[p].when {
			break
		}
		t[i] = t[p]
		i = p
	}
	if tmp != t[i] {
		t[i] = tmp
	}
}

func siftdownTimer(t []*timer, i int) {
	n := len(t)
	if i >= n {
		badTimer()
	}
	when := t[i].when
	tmp := t[i]
	for {
		c := i*4 + 1 // left child
		c3 := c + 2  // mid child
		if c >= n {
			break
		}
		w := t[c].when
		if c+1 < n && t[c+1].when < w {
			w = t[c+1].when
			c++
		}
		if c3 < n {
			w3 := t[c3].when
			if c3+1 < n && t[c3+1].when < w3 {
				w3 = t[c3+1].when
				c3++
			}
			if w3 < w {
				w = w3
				c = c3
			}
		}
		if w >= when {
			break
		}
		t[i] = t[c]
		i = c
	}
	if tmp != t[i] {
		t[i] = tmp
	}
}

// badTimer is called if the timer data structures have been corrupted,
// presumably due to racy use by the program. We panic here rather than
// panicing due to invalid slice access while holding locks.
// See issue #25686.
func badTimer() {
	throw("timer data corruption")
}
