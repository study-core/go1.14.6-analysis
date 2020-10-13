// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)


// todo WaitGroup 的实现
//
//
// A WaitGroup waits for a collection of goroutines to finish.
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// A WaitGroup must not be copied after first use.
type WaitGroup struct {
	noCopy noCopy  	// noCopy可以嵌入到结构中，在第一次使用后不可复制,使用go vet作为检测使用


	// 64-bit value: high 32 bits are counter, low 32 bits are waiter count.
	// 64-bit atomic operations require 64-bit alignment, but 32-bit
	// compilers do not ensure it. So we allocate 12 bytes and then use
	// the aligned 8 bytes in them as state, and the other 4 as storage
	// for the sema.

	// 64 bit：高32 bit是计数器，低32位是 阻塞的goroutine计数。
	// 64 bit 的原子操作需要64位的对齐，但是32位。
	// 编译器不能确保它,所以分配了12个byte对齐的8个byte作为状态。

	// state 中 三个元素分别代表:
	//
	//	counter： 		当前还未执行结束的goroutine计数器
	//	waiter count: 	等待goroutine-group结束的goroutine数量， 即有多少个等候者
	//	semaphore: 		信号量
	//
	//					32 bit	 32 bit     32 bit
	//					counter | waiter | semaphore
	//				   	      state 	 |  semaphore
	//
	//
	state1 [3]uint32

	// 拷贝waitgroup意味着对数组拷贝，golang的拷贝的都是值拷贝，数组的拷贝就是深拷贝了，并不是重新拷贝指针.
	// 理解了这点之后，我们就能发现，在拷贝之后，对waitgroup的操作完全是在两个waitgroup上了.
	// todo 所以 WaitGroup 不允许 拷贝哦


	/**
	WaitGroup对外提供三个接口：

			Add(delta int): 将delta值加到counter中
			Wait()： waiter递增1， 并阻塞等待信号量semaphore
			Done()： counter递减1， 按照waiter数值释放相应次数信号量
	 */
}

// 获取 state 和 semaphore 地址指针
//
// state returns pointers to the state and sema fields stored within wg.state1.
func (wg *WaitGroup) state() (statep *uint64, semap *uint32) {
	if uintptr(unsafe.Pointer(&wg.state1))%8 == 0 {
		return (*uint64)(unsafe.Pointer(&wg.state1)), &wg.state1[2]
	} else {
		return (*uint64)(unsafe.Pointer(&wg.state1[1])), &wg.state1[0]
	}
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// Typically this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// See the WaitGroup example.
//
//
//
// Add()做了两件事，
// 			一是    把delta值累加到counter中， 因为delta可以为负值， 也就是说counter有可能变成 0 或 负值
// 			第二件  就是当counter值变为0时， 跟据waiter数值释放等量的信号量， 把等待的goroutine全部唤醒， 如果counter变为负值， 则panic
func (wg *WaitGroup) Add(delta int) {
	statep, semap := wg.state()   // 获取 state 和 semaphore 地址指针
	if race.Enabled {
		_ = *statep // trigger nil deref early
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	state := atomic.AddUint64(statep, uint64(delta)<<32)    // 把delta左移32位累加到state， 即累加到counter中
	v := int32(state >> 32)			// 获取counter值
	w := uint32(state)				// 获取waiter值
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(semap))
	}

	// 经过累加后counter值变为负值， panic
	if v < 0 {
		panic("sync: negative WaitGroup counter")
	}


	if w != 0 && delta > 0 && v == int32(delta) {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}

	//	经过累加后， 此时， counter >= 0
	//	如果counter为正， 说明不需要释放信号量， 直接退出
	//	如果waiter为零， 说明没有等待者， 也不需要释放信号量， 直接退出
	if v > 0 || w == 0 {
		return
	}


	//
	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait,
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	//
	//
	//	当  waiter > 0时，此goroutine已将 counter 设置为0。
	//	现在不存在状态的并发突变：
	//		- 添加不得与Wait同时进行，
	//		- Wait看到counter == 0时，不会增加侍者的人数。
	//	仍然进行廉价的完整性检查以检测WaitGroup的滥用。
	//
	//
	if *statep != state {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}

	// 此时, counter 一定等于0, 而waiter一定大于0 (内部维护waiter, 不会出现小于0的情况),  先把counter置为0， 再释放waiter个数的信号量
	// Reset waiters count to 0.
	*statep = 0
	for ; w != 0; w-- {
		runtime_Semrelease(semap, false, 0)  // 释放信号量， 执行一次释放一个， 唤醒一个等待者
	}
}

// Done decrements the WaitGroup counter by one.
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait()方法也做了两件事
// 		一是累加waiter
// 		二是阻塞等待信号量
//
// Wait blocks until the WaitGroup counter is zero.
func (wg *WaitGroup) Wait() {
	statep, semap := wg.state()   // 获取state和semaphore地址指针
	if race.Enabled {
		_ = *statep // trigger nil deref early
		race.Disable()
	}
	for {
		state := atomic.LoadUint64(statep)				// 获取state值
		v := int32(state >> 32)							// 获取counter值
		w := uint32(state)								// 获取waiter值

		// 如果counter值为0, 说明所有goroutine都退出了, 不需要待待, 直接返回
		if v == 0 {
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}

		// 使用CAS（比较交换算法） 累加waiter, 累加可能会失败， 失败后通过 for loop下次重试
		// Increment waiters count.
		if atomic.CompareAndSwapUint64(statep, state, state+1) {				// CAS算法保证有多个goroutine同时执行Wait()时也能正确累加waiter
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(semap))
			}
			runtime_Semacquire(semap)   // 累加成功后， 等待信号量唤醒自己
			if *statep != 0 {
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}
