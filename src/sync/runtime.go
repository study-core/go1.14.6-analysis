// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire waits until *s > 0 and then atomically decrements it.
// It is intended as a simple sleep primitive for use by the synchronization
// library and should not be used directly.
func runtime_Semacquire(s *uint32)

// SemacquireMutex is like Semacquire, but for profiling contended Mutexes.
// If lifo is true, queue waiter at the head of wait queue.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_SemacquireMutex's caller.
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)

// Semrelease atomically increments *s and notifies a waiting goroutine
// if one is blocked in Semacquire.
// It is intended as a simple wakeup primitive for use by the synchronization
// library and should not be used directly.
// If handoff is true, pass count directly to the first waiter.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_Semrelease's caller.
func runtime_Semrelease(s *uint32, handoff bool, skipframes int)

// Approximation of notifyList in runtime/sema.go. Size and alignment must
// agree.
//
//
// todo 结构 必须要和 runtime/sema.go 中 notifyList 一致
type notifyList struct {
	wait   uint32
	notify uint32
	lock   uintptr
	head   unsafe.Pointer
	tail   unsafe.Pointer
}

// sync.Cond 的方法, 将当前G 加入等待队列
//
// 真正实现在 sema.go 的 runtime.notifyListAdd()
// See runtime/sema.go for documentation.
func runtime_notifyListAdd(l *notifyList) uint32


// sync.Cond 的方法, 根据票号t, 将当前G挂起,放入等待队列中
//
// 真正实现在 sema.go 的 runtime.notifyListWait()
// See runtime/sema.go for documentation.
func runtime_notifyListWait(l *notifyList, t uint32)

// sync.Cond 的方法, 将当前 wait 队列中所有 被阻塞的G 唤醒
//
// 真正实现在 sema.go 的 runtime.notifyListNotifyAll()
// See runtime/sema.go for documentation.
func runtime_notifyListNotifyAll(l *notifyList)

// sync.Cond 的方法, 唤醒下一个需要被唤醒的G
//
// 真正实现在 sema.go 的 runtime.notifyListNotifyOne()
// See runtime/sema.go for documentation.
func runtime_notifyListNotifyOne(l *notifyList)

// 用来检验 当前 runtime.go 中的 notifyList 结构体 和 sema.go 的 notifyList结构体 是否 结构和对齐 一致
//
// 真正实现在 sema.go 的 runtime.notifyListCheck()
// Ensure that sync and runtime agree on size of notifyList.
func runtime_notifyListCheck(size uintptr)

func init() {
	var n notifyList
	runtime_notifyListCheck(unsafe.Sizeof(n))   // 用来检验 当前 runtime.go 中的 notifyList 结构体 和 sema.go 的 notifyList结构体 是否 结构和对齐 一致
}

// Active spinning runtime support.
// runtime_canSpin reports whether spinning makes sense at the moment.
func runtime_canSpin(i int) bool    // 是否可以自旋

// runtime_doSpin does active spinning.
func runtime_doSpin()		// 开始自旋

// 获取系统纳秒时间戳
// 在 `sema.go 的 sync_nanotime() 实现`
func runtime_nanotime() int64
