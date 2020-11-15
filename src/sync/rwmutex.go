// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// todo REMutex 读写锁结构定义

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
type RWMutex struct {

	// 互斥锁 引用
	w           Mutex  // held if there are pending writers

	// 写阻塞等待的信号量， 最后一个读者释放锁时会释放信号量
	writerSem   uint32 // semaphore for writers to wait for completing readers
	// 读阻塞的协程等待的信号量， 持有写锁的协程释放锁后会释放信号量
	readerSem   uint32 // semaphore for readers to wait for completing writers
	// 记录读者个数  todo [读锁 和 写锁 间相互判断的数值]
	readerCount int32  // number of pending readers
	// 记录写阻塞时，读者个数		todo 只是作为 写锁被阻塞时的 读锁计数器, 当这个为0时, 才会释放之前被挂起的写锁 g
	readerWait  int32  // number of departing readers
}

const rwmutexMaxReaders = 1 << 30    // 读锁的上限,  1,073,741,824 个  (100+亿个)

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.

// RLock锁定rw以进行读取。
//
// 不应将其用于递归读取锁定； 被写锁定的lock调用将使新读者无法获得锁定。
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}

	// todo 自增 读锁计数器
	// 每次goroutine获取读锁时，readerCount+1
	//  todo 如果写锁已经被获取，那么readerCount在 - rwmutexMaxReaders与 0 之间 (查看 Lock() 部分代码)，这时挂起获取读锁的goroutine，
	// 如果写锁没有被获取，那么readerCount>=0，获取读锁,不阻塞
	// todo 通过readerCount的正负判断读锁与写锁互斥,  如果有写锁存在就挂起读锁的goroutine,多个读锁可以并行
	if atomic.AddInt32(&rw.readerCount, 1) < 0 { // 说明目前有 写锁
		// A writer is pending, wait for it.
		//
		// 挂起当前 来获取读锁的 g
		runtime_SemacquireMutex(&rw.readerSem, false, 0)  // 将当前 读锁代码 停留在 这一行逻辑, 等待  runtime_Semrelease() 来释放 ...
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.

// 释放读锁
//
// 读锁不会影响其他读操作
// 如果在进入RUnlock时没有锁没有被施加读锁的话，则会出现运行时错误。
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}

	// todo 读锁计数器 -1
	// 有四种情况，其中后面三种都会进这个 if
	//  todo 【有读锁】
	// 		【一】有读锁，没有写锁被挂起
	// 		【二】有读锁，且也有写锁被挂起
	//  todo 【没有读锁】
	// 		【三】没有读锁且没有写锁被挂起的时候， r+1 == 0
	// 		【四】没有读锁但是有写锁被挂起，则 r+1 == -(1 << 30)
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 {
		// Outlined slow-path to allow the fast-path to be inlined  概述慢速路径以允许快速路径内联
		rw.rUnlockSlow(r)
	}
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {

	// todo 【没有读锁】,  抛 panic
	// 读锁早就被没有了，那么在此 -1 是需要抛异常的
	// 这里只有当读锁没有的时候才会出现的两种极端情况
	// 【一】没有读锁且没有写锁被挂起的时候， r+1 == 0
	// 【二】没有读锁但是有写锁被挂起，则 r+1 == -(1 << 30)
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		race.Enable()
		throw("sync: RUnlock of unlocked RWMutex")
	}
	// todo 【有读锁】
	//			且没有写锁, 不做任何事了...


	// A writer is pending.
	//
	// 有写锁, 阻塞时, 递减 写锁 阻塞时的 读锁计数器,
	//	当 完全没有 读锁时, 释放 挂起的 写锁 g
	if atomic.AddInt32(&rw.readerWait, -1) == 0 {
		// The last reader unblocks the writer.
		runtime_Semrelease(&rw.writerSem, false, 1)  // 释放掉 之前被 runtime_SemacquireMutex() 挂起的 那个写锁
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.

// 对一个已经lock的rw上锁会被阻塞
// 如果锁已经锁定以进行读取或写入，则锁定将被阻塞，直到锁定可用。
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	//
	// todo 首先, 当前 g 先获取 互斥锁    （主要是为了 互斥 其他 g 竞争 写锁）
	rw.w.Lock()
	// Announce to readers there is a pending writer.
	//
	// todo 通知 所有 读锁, 当前有一个 阻塞的 写锁

	// 读锁数目
	//
	// 下面这个 骚操作  todo 先将 readCount 的数值 改变, 然后 再用改变后的 readCount + rwmutexMaxReaders 得到 临时变量 r 去做判断
	//					todo 但是 此时的  readCount 的值 已经是 更改了的, 只有在  RUnlock  RLock  Unlock 三个动作中被变更.
	//					todo 这么做事 有意义的, 你细品 ......
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders

	// Wait for active readers.
	//
	// todo 当 读锁数目 不为0 时
	// 记录 写锁阻塞时的 读锁数目, 且 将当前获取写锁的 g 挂起
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
		runtime_SemacquireMutex(&rw.writerSem, false, 0)  // 当前这个写锁的 G 的逻辑停留在这一行了, 等待 runtime_Semrelease() 释放 ...
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.

// Unlock 已经Unlock的锁会被阻塞.
// 如果在写锁时，rw没有被解锁，则会出现运行时错误。
//
// 与互斥锁一样，锁定的RWMutex与特定的goroutine无关。
// 一个goroutine可以RLock（锁定）RWMutex然后安排另一个goroutine到RUnlock（解锁）它
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	//
	// todo 和 `Lock` 中的动作 相反, 主要是讲 之前减掉的数 加回来, 并以此 间接通知 所有 读锁, 目前已经没有写锁了
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders)

	// todo 在存在 读锁时, 释放 写锁, 需要 panic
	if r >= rwmutexMaxReaders {
		race.Enable()
		throw("sync: Unlock of unlocked RWMutex")
	}


	// Unblock blocked readers, if any.
	//
	// todo 唤醒 获取【读锁】期间所有被阻塞的goroutine
	for i := 0; i < int(r); i++ {
		runtime_Semrelease(&rw.readerSem, false, 0)  // 将所有 读锁的逻辑，从  runtime_SemacquireMutex() 出继续往下走.
	}
	// Allow other writers to proceed.
	//
	// 释放掉 写锁
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
