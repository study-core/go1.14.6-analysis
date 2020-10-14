// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package time

// Sleep pauses the current goroutine for at least the duration d.
// A negative or zero duration causes Sleep to return immediately.
func Sleep(d Duration)		// runtime.timeSleep() 才是真正的实现


// todo 真正的定时器底层实现
//
//   todo 这个 结构体的 类型, 必须要和  runtime/time.go 中的 timer 结构体结构一致
// Interface to timers implemented in package runtime.
// Must be in sync with ../runtime/time.go:/^type timer
type runtimeTimer struct {
	pp       uintptr   // 被关联该 timer 的 P的地址
	when     int64
	period   int64

	// 一个 定时器的回调函数
	f        func(interface{}, uintptr) // NOTE: must not be closure      // 必须不能是  闭包

	// 传递给  f 回调函数的 第一个参数
	arg      interface{}
	seq      uintptr  // 传递给 f 回调函数的 第二个参数
	nextwhen int64
	status   uint32
}

// when is a helper function for setting the 'when' field of a runtimeTimer.
// It returns what the time will be, in nanoseconds, Duration d in the future.
// If d is negative, it is ignored. If the returned value would be less than
// zero because of an overflow, MaxInt64 is returned.
//
//
// when	是用于设置 runtimeTimer的 "when" 字段的辅助函数。
//		它返回未来的持续时间d（以纳秒为单位）的时间。
//		如果d为负，则将其忽略。 如果由于溢出而返回的值小于零，则返回MaxInt64。
func when(d Duration) int64 {
	if d <= 0 {
		// 入参的【时长】  小于 0 , 取 当前 纳秒时间
		return runtimeNano()
	}
	// 否则, 当前纳秒 + 时长
	t := runtimeNano() + int64(d)
	if t < 0 {
		t = 1<<63 - 1 // math.MaxInt64
	}
	return t
}

func startTimer(*runtimeTimer)				// runtime.startTimer() 才是真正的实现
func stopTimer(*runtimeTimer) bool       	// runtime.stopTimer() 才是真正的实现
func resetTimer(*runtimeTimer, int64)		// runtime.resetTimer() 才是真正地实现

// todo 定时器的底层实现     定时器的定义   Timer的定义
//
// The Timer type represents a single event.
// When the Timer expires, the current time will be sent on C,
// unless the Timer was created by AfterFunc.
// A Timer must be created with NewTimer or AfterFunc.
//
//
// Timer类型代表一个事件
//		定时器到期时，除非定时器是由 AfterFunc 创建的，否则当前时间将在C上发送
//		必须使用 NewTimer 或 AfterFunc 创建一个计时器
type Timer struct {
	C <-chan Time
	r runtimeTimer
}

//	todo 终止定时器  终止timer
//
//		true: 定时器超时前停止， 后续不会再有事件发送
//		false: 定时器超时后停止
// Stop prevents the Timer from firing.
// It returns true if the call stops the timer, false if the timer has already
// expired or been stopped.
// Stop does not close the channel, to prevent a read from the channel succeeding
// incorrectly.
//
// To ensure the channel is empty after a call to Stop, check the
// return value and drain the channel.
// For example, assuming the program has not received from t.C already:
//
// 	if !t.Stop() {
// 		<-t.C
// 	}
//
// This cannot be done concurrent to other receives from the Timer's
// channel or other calls to the Timer's Stop method.
//
// For a timer created with AfterFunc(d, f), if t.Stop returns false, then the timer
// has already expired and the function f has been started in its own goroutine;
// Stop does not wait for f to complete before returning.
// If the caller needs to know whether f is completed, it must coordinate
// with f explicitly.
func (t *Timer) Stop() bool {
	if t.r.f == nil {
		panic("time: Stop called on uninitialized Timer")
	}
	return stopTimer(&t.r)
}


// todo 长长的实例化一个 timer 对象
// NewTimer creates a new Timer that will send
// the current time on its channel after at least duration d.
func NewTimer(d Duration) *Timer {
	c := make(chan Time, 1)
	t := &Timer{
		C: c,
		r: runtimeTimer{
			when: when(d),  // when 代表 timer 触发的绝对时间。计算方式就是当前时间加上延时时间
			f:    sendTime,
			arg:  c,
		},
	}

	// 启动 runtimeTimer
	startTimer(&t.r)
	return t
}

// 重置定时器  重置timer
//
// Reset changes the timer to expire after duration d.
// It returns true if the timer had been active, false if the timer had
// expired or been stopped.
//
// Reset should be invoked only on stopped or expired timers with drained channels.
// If a program has already received a value from t.C, the timer is known
// to have expired and the channel drained, so t.Reset can be used directly.
// If a program has not yet received a value from t.C, however,
// the timer must be stopped and—if Stop reports that the timer expired
// before being stopped—the channel explicitly drained:
//
// 	if !t.Stop() {
// 		<-t.C
// 	}
// 	t.Reset(d)
//
// This should not be done concurrent to other receives from the Timer's
// channel.
//
// Note that it is not possible to use Reset's return value correctly, as there
// is a race condition between draining the channel and the new timer expiring.
// Reset should always be invoked on stopped or expired channels, as described above.
// The return value exists to preserve compatibility with existing programs.
func (t *Timer) Reset(d Duration) bool {
	if t.r.f == nil {
		panic("time: Reset called on uninitialized Timer")
	}

	// 时间转换
	w := when(d)

	// todo 先停掉  定时器 然后再启动
	active := stopTimer(&t.r)			// 停掉
	resetTimer(&t.r, w)					// 重置

	return active
}

func sendTime(c interface{}, seq uintptr) {
	// Non-blocking send of time on c.
	// Used in NewTimer, it cannot block anyway (buffer).
	// Used in NewTicker, dropping sends on the floor is
	// the desired behavior when the reader gets behind,
	// because the sends are periodic.
	select {
	case c.(chan Time) <- Now():
	default:   // todo 这一行保证了 select 不会被阻塞      (特别是在 Ticker 的实现中用到这个了)   Ticker 也是用这个 sendTime() 函数的
	}
}

// todo 包装了一下 实例化timer实例
//
// After waits for the duration to elapse and then sends the current time
// on the returned channel.
// It is equivalent to NewTimer(d).C.
// The underlying Timer is not recovered by the garbage collector
// until the timer fires. If efficiency is a concern, use NewTimer
// instead and call Timer.Stop if the timer is no longer needed.
func After(d Duration) <-chan Time {
	return NewTimer(d).C
}


// todo 和 直接实例化一个 timer 类似,
//			只不过 回调函数 f 用的不是  `sendTime()`, 而是我们自己定义的 回调func
//
// AfterFunc waits for the duration to elapse and then calls f
// in its own goroutine. It returns a Timer that can
// be used to cancel the call using its Stop method.
func AfterFunc(d Duration, f func()) *Timer {
	t := &Timer{
		r: runtimeTimer{
			when: when(d),
			f:    goFunc, // 我们自己定义的 函数
			arg:  f,
		},
	}
	startTimer(&t.r)
	return t
}

func goFunc(arg interface{}, seq uintptr) {
	go arg.(func())()
}
