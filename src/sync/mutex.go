// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// todo 互斥锁 结构实现   mutex 结构实现
// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	// 将一个32位整数拆分为
	//  `当前阻塞的goroutine数目(29位)|饥饿状态(1位)|唤醒状态(1位)|锁状态(1位)`   的形式，来简化字段设计
	state int32     		// 表示互斥锁的状态， 比如是否被锁定等      todo Mutex.state是32位的整型变量， 内部实现时把该变量分成四份， 用于记录Mutex的四种状态:
	sema  uint32			// 表示信号量， 协程阻塞 等待该信号量， 解锁的协程 释放信号量 从而唤醒等待信号量的协程   todo 使用他 来标识 内存中 当前 mutex 所在的位置，看 sema.go的 `semroot()` 取了什么出来 (真正的 mutex是 runtime2.go中的mutex)
}

/**
Locked: 	表示该Mutex是否已被锁定， 0： 没有锁定 1： 已被锁定
Woken: 		表示是否 有协程已被唤醒， 0： 没有协程唤醒 1： 已有协程唤醒， 正在加锁过程中 【注意： 未被唤醒并不是指 休眠，而是指为了让所能被设置 被唤醒的一个初始值】
Starving： 	表示该Mutex是否处理饥饿状态， 0： 没有饥饿 【正常模式】 1： 饥饿状态 【饥饿模式】， 说明有协程阻塞了超过 1ms
Waiter: 	表示阻塞等待锁的协程个数， 协程解锁时根据此值来判断是否需要释放信号量

todo 协程之间抢锁 实际上是【抢给Locked赋值的权利】， 能给Locked域置1， 就说明抢锁成功。 抢不到的话就阻塞等待 Mutex.sema信号量， 一旦持有锁的协程解锁， 等待的协程会依次被唤醒。
 */

// A Locker represents an object that can be locked and unlocked.  `Locker` 表示可以锁定和解锁的对象   锁的接口定义
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // mutex is locked				//	1<<0 ==>  0001  ==> 1
	mutexWoken												//	1<<1 ==>  0010  ==> 2
	mutexStarving											//	1<<2 ==>  0100  ==> 4
	mutexWaiterShift = iota									// 	3

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	/**
	 互斥公平性

	 	todo Mutex可以处于两种操作模式：【正常】和 【饥饿】


  		todo 在【正常】模式下:
			等待的goroutines按照FIFO（先进先出）顺序排队，但是goroutine被唤醒之后并不能立即得到mutex锁，它需要与新到达的goroutine争夺mutex锁。
   			因为新到达的goroutine已经在CPU上运行了，所以被唤醒的goroutine很大概率是争夺mutex锁是失败的。出现这样的情况时候，被唤醒的goroutine需要 继续排队在队列的前面。
   			如果被唤醒的goroutine有超过 `1ms` 没有获取到mutex锁，那么它就会变为饥饿模式。

   		todo 在【饥饿】模式下:
			mutex锁直接从解锁的goroutine交给队列前面的 等待的goroutine [因为 队列前面的 goroutine 极可能是饥饿了]。
			新达到的goroutine也不会去争夺mutex锁 （即使没有锁，也不能去自旋） ，而是到等待 goroutine 队列尾部排队。
   			锁的所有权将从 unlock 的 gorutine 直接交给交给 等待队列中的第一个 goroutine
			新来的goroutine将不会尝试去获得锁，即使锁看起来是 unlock状态, 也不会去尝试自旋操作，而是放在等待队列的尾部。
			如果有一个等待的goroutine获取到mutex锁了，
		todo 【饥饿】 切回 【正常】 的条件:
			如果它满足下条件中的任意一个，mutex将会切换回去正常模式：

   					1. 是等待队列中的最后一个goroutine
   					2. 当前g的等待时间不超过1ms。  todo 因为当前 g 之前可能是最饥饿的那么, 但是他的 等待时间又不超过 1 ms 所以 和 事实相违背啊?? 往下看吧    （当前只有一个协程在占用锁）

	todo 【注意】

	正常模式：有更好的性能，因为goroutine可以连续多次获得mutex锁；
    饥饿模式：能阻止尾部延迟的现象，对于预防队列尾部goroutine一直无法获取mutex锁的问题。

	todo [自旋锁（spinlock）：是指当一个线程在获取锁的时候，如果锁已经被其它线程获取，那么该线程将循环等待，然后不断的判断锁是否能够被成功获取，直到获取到锁才会退出循环。]
			自旋锁是为实现保护共享资源而提出一种锁机制。
			其实，自旋锁与互斥锁比较类似，它们都是为了解决对某项资源的互斥使用。
			无论是互斥锁，还是自旋锁，在任何时刻，最多只能有一个保持者，
			也就说，在任何时刻最多只能有一个执行单元获得锁。

			但是两者在调度机制上略有不同。
			todo [互斥锁] :
					如果资源已经被占用，资源申请者只能进入睡眠状态.
			todo [自旋锁] :
					不会引起调用者睡眠，如果自旋锁已经被别的执行单元保持，调用者就一直循环在那里看是否该自旋锁的保持者已经释放了.
   */
	starvationThresholdNs = 1e6							//	1000000    1ms
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
//
// 如果锁已经在使用中，则调用goroutine 直到互斥锁可用为止。
/**
在此之前我们必须先说下 四个重要的方法；
todo 【runtime_canSpin】，【runtime_doSpin】，【runtime_SemacquireMutex】，【runtime_Semrelease】

		todo 【runtime_canSpin】：
							在 `src/runtime/proc.go` 中被实现 【sync_runtime_canSpin】； 表示 比较保守的自旋，
							golang中自旋锁并不会一直自旋下去，在runtime包中runtime_canSpin方法做了一些限制,
							传递过来的 iter大等于4  或者 cpu核数小等于1，最大逻辑处理器大于1，至少有个本地的P队列，
							并且本地的P队列可运行G队列为空。

		todo 【runtime_doSpin】：
							在 `src/runtime/proc.go` 中被实现 【sync_runtime_doSpin】；表示 会调用 `procyield()` 函数，
							该函数也是汇编语言实现。函数内部循环调用PAUSE指令。PAUSE指令什么都不做，
							但是会消耗CPU时间，在执行PAUSE指令时，CPU不会对它做不必要的优化。

		todo 【runtime_SemacquireMutex】：
							在 `src/runtime/sema.go` 中被实现 【sync_runtime_SemacquireMutex】；表示通过信号量 阻塞当前协程

		todo 【runtime_Semrelease】:
							在 `src/runtime/sema.go` 中被实现 【sync_runtime_Semrelease】
*/
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 如果m.state为 0，说明当前的对象还没有被锁住，进行原子性赋值操作设置为mutexLocked状态，CompareAnSwapInt32返回true
	// 否则说明对象已被其他goroutine锁住，不会进行原子赋值操作设置，CopareAndSwapInt32返回false
	/**
	如果mutext的state没有被锁，也没有等待/唤醒的goroutine, 锁处于正常状态，那么获得锁，返回.
    比如锁第一次被goroutine请求时，就是这种状态。或者锁处于空闲的时候，也是这种状态
	 */
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}

	/** 在 锁定没有成功的时候，才会往下面走 */

	// 首先判断是否已经加锁并处于 正常模式，
	// 将原先锁的 state & (1 和 4 | 的结果，目的就是为了检验 state 是处于 1 还是 4 状态， 还是两者都是.
	// 如果与1相等，则说明此时处于 正常模式并且已经加锁，而后判断当前协程是否可以自旋。
	// 如果可以自旋，则通过右移三位判断是否还有协程正在等待这个锁，
	// 如果有，并通过 低2位 判断是否该所处于被唤醒状态，
	// 如果并没有，则将其状态量设为被唤醒的状态，之后进行自旋，直到该协程自旋数量达到上限，
	// 或者当前锁被解锁，
	// 或者当前锁已经处于 饥饿模式

	// Slow path (outlined so that the fast path can be inlined)    慢速路径（已概述，以便可以内联快速路径）
	m.lockSlow()
}

func (m *Mutex) lockSlow() {

	// 标记 当前goroutine的等待时间
	// 开始等待时间戳  todo 主要用来及时 是否有 g 等待超过 1ms,  判断是否将 lock 置为 【饥饿状态】
	var waitStartTime int64

	// 当前goroutine是否已经处于 【饥饿状态】
	// 饥饿模式标识 true: 饥饿  false: 未饥饿
	starving := false

	// 当前 goroutine是否已唤醒
	// 被唤醒标识  true: 被唤醒   flase: 未被唤醒
	awoke := false

	// 当前 goroutine 自旋次数
	iter := 0

	// 保存当前对象锁状态，做对比用
	old := m.state

	// for 来实现 CAS(Compare and Swap) 非阻塞同步算法 (对比交换)
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		//
		// 不要在 【饥饿模式】 下 自旋，因为在 【饥饿模式下】lock 的所有权会 被优先移交给 等待队列中首部的 goroutine，因此我们 无论如何都无法获取 lock
		//
		//
		// 相当于 xxxx...x0xx & 0101 = 01，当前对象锁被使用
		// old & (是否锁定|是否饥饿) == 是否锁定 todo 如果 old 中存在 饥饿 状态， 那么 此condition 不成立
		// runtime_canSpin() 表示 是否可以自旋。runtime_canSpin返回true，可以自旋。即： 判断当前goroutine是否可以进入自旋锁
		/**
		第一个条件：是state已被锁，但是不是饥饿状态。如果时饥饿状态，自旋时没有用的，锁的拥有权直接交给了 等待队列的第一个 goroutine。
        第二个条件：是还可以自旋，多核、压力不大并且在一定次数内可以自旋， 具体的条件可以参考`sync_runtime_canSpin`的实现。
        如果满足这两个条件，不断自旋来等待锁被释放、或者进入饥饿状态、或者不能再自旋。
		 */
		// todo 下面是 判断 当前 g  是否需要 进入 自旋状态
		 // 如果 只有被锁状态 且 可以 当前来获取 mutex 的 g 可以自旋 时, 我们进行 当前 g 的自旋
		 //
		 // todo 只有 不是 【饥饿】 且 已被锁定 且 可以自旋 时,  才做 自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			/**
			todo 主动 自旋 是有意义的。试着设置 mutexWoken （锁唤醒）标志，告知解锁，不唤醒其他阻塞的goroutines
			 */
			// old&mutexWoken == 0 再次确定是否被唤醒： xxxx...xx0x & 0010 = 0
			// old>>mutexWaiterShift != 0 查看是否有goroution在排队
			// tomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) 将对象锁在老状态上追加唤醒状态：xxxx...xx0x | 0010 = xxxx...xx1x

			// 如果当前 g 标识位 awoke为 未被唤醒 && （old 也为 未被唤醒） && 有正在等待的 goroutine && 则修改 old 为 被唤醒成功
			// 则修改标识位 awoke 为 true 被唤醒
			/**
			自旋的过程中如果发现state还没有设置woken标识，则设置它的woken标识， 并标记自己为被唤醒。
			 */

			 // CompareAndSwapInt32函数 在被调用之后会先判断参数addr指向的被操作值与参数old的值是否相等.
			 // 仅当此判断得到肯定的结果之后，该函数才会用参数new代表的新值替换掉原先的旧值。否则，后面的替换操作就会被忽略.
			 //
			 // todo 如果当前 g 不是 刚被唤醒, 且 等待队列中有 g, 则 将当前 g 设置为 唤醒, 而不通知 其他 等待队列中的 g         (就是 优先 让自己 获取锁)
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true     // todo  当前g 被唤醒, 且 mutex 追加了 【被唤醒 标识】,  这些都会在 下面 被清除掉
			}

			// 进入自旋锁 后当前goroutine并不挂起，仍然在占用cpu资源，所以重试一定次数后，不会再进入自旋锁逻辑
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}

		// todo 以下代码是不使用 【自旋】 的情况

		/**
		 到了这一步， state的状态可能是：
         	1. 锁还没有被释放，锁处于正常状态
         	2. 锁还没有被释放， 锁处于饥饿状态
         	3. 锁已经被释放， 锁处于正常状态
         	4. 锁已经被释放， 锁处于饥饿状态
         并且 当前gorutine的 awoke可能是true, 也可能是false (是 false时, 其它goroutine已经设置了state的woken标识)
         new 复制 state的当前状态， 用来设置新的状态 todo 先将 old 拷贝出来, 然后在 old的之上做更改, 而不影响 old变量原来的值, old变量后面还要作比较用呢
         old 是锁当前的状态
		 */

		new := old

		/** 下面的几个 if 分别是并列语句，来判断如给 设置 state 的new 状态 */

		/**
		如果old state状态不是饥饿状态, new state 设置锁， 尝试通过CAS获取锁,
        如果old state状态是饥饿状态, 则不设置new state的锁，因为饥饿状态下锁直接转给等待队列的第一个.
		 */
		// 不要试图获得饥饿goroutine的互斥锁，新来的goroutines必须排队。
		// 对象锁饥饿位被改变 为 1 ，说明处于饥饿模式
		// xxxx...x0xx & 0100 = 0xxxx...x0xx




		// Don't try to acquire starving mutex, new arriving goroutines must queue.  不要试图获取 饥饿的 mutex，新来的goroutine必须排队到 等待队列末尾
		/**
		todo 【一】如果是正常状态 （如果是正常，则 可以 可能竞争到锁）       为什么是可能？ 因为 进到这里的 一般是 old 原来的值中就已经有 lock 状态了, 所以我们 |= 去操作一点都不影响原来额状态
		*/
		if old&mutexStarving == 0 {
			// xxxx...x0x0 | 0001 = xxxx...x0x1，或者 xxxx...x0x1 | 0001 = xxxx...x0x1  将标识对象锁被锁住
			new |= mutexLocked
		}

		/**
		todo 【二】处于饥饿 或者 锁被占用 状态下
		*/
		if old&(mutexLocked|mutexStarving) != 0 {
			// xxxx...x1x1 & (0001 | 0100) => xxxx...x1x1 & 0101 != 0      或者 xxxx...x0x1 & (0001 | 0100) => xxxx...x0x1 & 0101 != 0       或者 xxxx...x1x0 & (0001 | 0100) => xxxx...x1x0 & 0101 != 0
			//
			// 当前mutex处于饥饿模式并且锁已被占用，todo 新加入进来的goroutine放到队列后面，所以 等待者数目 +1
			new += 1 << mutexWaiterShift
		}


		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 当前的goroutine将mutex 转换为饥饿模式。但是，如果互斥锁当前没有解锁，就不要进行切换,设置mutex状态为饥饿模式。Unlock预期有饥饿的goroutine
		/**
		如果之前由于自旋而将该锁唤醒，那么此时将其低二位的状态量重置为0 (即 未被唤醒)。
		之后判断starving是否为true，如果为true说明在上一次的循环中，
		锁需要被定义为 饥饿模式，那么在这里就将相应的状态量低3位设置为1表示进入饥饿模式
		*/
		/***
		todo 【三】如果 当前goroutine已经处于饥饿状态 （表示当前 goroutine 的饥饿标识位 starving）， 并且old state的已被加锁,
        将new state的状态标记为饥饿状态, 将锁转变为饥饿状态.
		 */
		// 当前的goroutine将 mutex 为饥饿模式。但是，如果 mutex 当前没有解锁，就不要进行切换, 直接设置mutex状态为饥饿模式。Unlock预期有饥饿的goroutine
		//
		//
		// old&mutexLocked != 0  xxxx...xxx1 & 0001 != 0 锁已经被占用
		//
		// 当前 g 处于 【饥饿】 且 mutex 还处于被锁定
		if starving && old&mutexLocked != 0 {
			// 给 mutex 追加 【饥饿】状态
			new |= mutexStarving
		}

		/**
		todo 【四】 如果本goroutine已经设置为唤醒状态
		需要清除 new state的唤醒标记, 因为本goroutine 要么获得了锁，要么进入休眠，
        总之state的 新状态不再是woken状态.
		 */
		// 如果 当前goroutine已经被唤醒，因此需要在两种情况下重设标志
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			// 当前 goroutine已从 睡眠状态 被唤醒，因此无论哪种情况，我们都需要 清除 mutex 的 【被唤醒标志】
			if new&mutexWoken == 0 {  // todo  因为只要有一个 g 被唤醒, 那么 new 的标识 上都会有 【被唤醒 标识】
				// xxxx...xx0x & 0010 == 0,如果唤醒标志为与awoke的值不相协调就 panic
				// 即 当前 g 为 被唤醒 而 mutex.state 为 未被唤醒 时, panic
				throw("sync: inconsistent mutex state")
			}

			// 清除 mutex 上的 被唤醒 标识
			new &^= mutexWoken
		}

		/**
		todo 之后尝试通过cas将 new 的state状态量赋值给state

		如果失败，则 重新获得其 state在下一步循环重新重复上述的操作。
		如果成功，首先判断已经阻塞时间 (通过 标记本goroutine的等待时间 waitStartTime )，如果为零，则从现在开始记录
		*/

		// 将新的状态赋值给 state
		// 注意new的 锁标记不一定是 1, 也可能只是标记一下锁的state是饥饿状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) { // 将 mutex 的状态 修改成功后.


			/**
			 todo 如果old state的状态是  未被锁状态，并且锁   未饥饿状态,
             todo 那么当前goroutine 可以追加到队列 末尾了
			 */
			// xxxx...x0x0 & 0101 = 0，表示可以获取对象锁 （即 还是判断之前的状态，锁不是饥饿 也不是被锁定， 将当前 g 追加到队列末尾, 结束流程）

			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}


			// todo 下面都是 当前 g 还没抢到锁的操作
			//
			// todo  以下的操作都是为了判断是否从【饥饿模式】中恢复为【正常模式】

			// 在正常模式下，等待的goroutines按照FIFO（先进先出）顺序排队,
			// 如果当前 g 被唤醒了, 但是 却抢不到锁 (mutex可能被新进来的 g 抢到了, 因为新进来的g很活跃,已经是正在cpu上运行的,所以可能优先抢到了)
			//		这时候当前g被排到 等待队列的前面去, 且开始 进入 等待计时 (后面判断是否需要 切换成 【饥饿】模式)


			// If we were already waiting before, queue at the front of the queue.  如果 我们之前已经在等待，请在队列的最前面排队
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()  // 开始 进入 等待计时
			}


			// 通过runtime_SemacquireMutex()通过信号量将当前协程阻塞
			// 函数 runtime_SemacquireMutex 定义在 sema.go
			/**
			既然未能获取到锁， 那么就使用 [sleep原语] 阻塞 当前goroutine
            如果是新来的goroutine, queueLifo = false, 加入到等待队列的尾部，耐心等待
            如果是唤醒的goroutine, queueLifo = true, 加入到等待队列的头部
			 */
			// 通过 信号量 阻塞当前 g
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)


			// 当之前调用 runtime_SemacquireMutex 方法将 当前 争夺锁的 g 挂起后，如果 g 被唤醒，那么就会继续下面的流程

			/**
			使用 [sleep原语] 之后， 当前goroutine被唤醒
            计算当前goroutine是否已经处于饥饿状态.
			 */
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs   // 当前 g 是否已经是 饥饿 或者 是否等待超过 1 ms
			old = m.state

			/**
			todo 如果 当前的 mutex.state已经是【饥饿状态】
            那么锁应该处于Unlock状态，那么应该是锁被直接交给了本goroutine
			 */
			// xxxx...x1xx & 0100 != 0  处于 饥饿状态
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 如果 当前goroutine被唤醒，并且 mutex 处于饥饿模式，
				// 则  mutex的所有权已移交给 当前 g，但 mutex处于某种不一致的状态：MutexLocked未设置，我们仍然被视为 等待队列中的一员,
				// 则需要修复这个问题. 抛panic

				/**
				 如果当前的state已被锁，或者已标记为唤醒， 或者等待的队列中为空,
                 那么state是一个非法状态
				  */
				// xxxx...xx11 & 0011 != 0
				// mutex又可能是 被锁定，又可能是 被唤醒    或者 没有等待的goroutine
				// 那么  panic,
				// 因为 和 实际情况 相违背了, 这时候应该有 g 在等待的 , mutex.state 也是 要么被唤醒, 要么被锁定
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}

				// todo delta 表示当 一个 中转变量
				/**
				0001 << 3 == 1000

				delta = 0001 - 1000 = 1 - 8 == -7   （7 = 0111 所以 -7 是等于 减掉 0111）

				一个骚操作
				 */
				delta := int32(mutexLocked - 1<<mutexWaiterShift)

				// todo 如果当前 g 不是饥饿状态 或者   已经没有 其他等待的g <只有一个 当前 g 了>，就没有必要继续维持 饥饿模式，同时也没必要继续执行该循环（当前只有一个协程在占用锁）
				/**
				todo 如果 当前goroutine并不处于饥饿状态，或者它是最后一个等待者
                	那么我们需要把 锁的state状态设置为 【正常模式】.
				 */
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					//
					//退出饥饿模式
					//在此处进行操作并考虑等待时间至关重要
					//饥饿模式效率低下，一旦两个goroutine将 mutex 切换到饥饿模式，那么 它们就 可能无限地进行 锁定

					// -7 -4 = -11  （ 11 == 1011 == 1个 g 且 被唤醒 和 被锁定 ） todo 也就是说, 等待队列只有 一个 当前 g 了 且 当前 g 被唤醒 且获取了锁
					delta -= mutexStarving
				}
				// 将 【1个 g 且 被唤醒 和 被锁定】 状态 从  mutex 上清除掉
				// 设置新state, 因为已经获得了锁，退出、返回
				atomic.AddInt32(&m.state, delta)   // todo  想不要明白 为什么 把  lock 标识 也清掉了??  因为只有一个 g 了, 没人和我竞争, 所以直接清除掉 所有标识 ??  但是为啥不清空 【饥饿】 状态 ??
				break
			}
			/**
			如果当前的锁是【正常模式】， 当前goroutine被唤醒，自旋次数清零，从for循环开始处重新开始
			 */
			awoke = true	// 当前 g 被唤醒
			iter = 0   // 自旋次数 清空
		} else {

			// 如果CAS不成功，重新获取锁的state, 从for循环开始处重新开始 继续上述动作  todo 继续 尝试 获取锁
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
//
// todo 解锁 一个未被锁定的 mutex时，是会报错.
//
// todo 锁定的互斥锁 没有和 特定的goroutine 有关.
// todo 允许一个goroutine锁定Mutex， 然后安排另一个goroutine解锁它。    (注意的点)
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	/**
	todo 如果state不是处于 【被锁定】的状态, 那么就是Unlock根本没有加锁的mutex, 抛 panic
	*/
	// state -1 标识解锁 (移除锁定标记)
	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		// 概述了慢速路径，以允许内联快速路径。
		// 要在跟踪过程中隐藏unlockSlow，我们在跟踪GoUnblock时会跳过一帧。
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {

	/**
	释放了锁，还得需要通知其它在 等待队列中的 g.
	被通知的 goroutine 会去做下面的事情:

    	锁如果处于【饥饿状态】，直接交给等待队列的第一个, 唤醒它，让它去获取锁.
    	锁如果处于【正常状态】，则需要唤醒对头的 goroutine 让它和新来的goroutine去竞争锁，当然极大几率为失败，
								这时候 被唤醒的goroutine需要排队在队列的前面 (然后自旋， 并开始做 等待计时).
								如果被唤醒的goroutine有超过1ms没有获取到mutex锁，那么它就会变为饥饿模式.
	 */

	 // 这里先 校验下,  如果 刚刚释放锁成功, 那么 (xxx0 + 0001) & 0001 != 0
	 // 否则, 刚刚释放锁的动作是失败的,  肯定是 对 【已经是放过】的锁做了【再次释放】
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}

	// todo 锁处于【正常模式】
	// xxxx...x0xx & 0100 = 0 ;判断是否处于正常模式
	if new&mutexStarving == 0 {

		// 记录缓存值
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			//
			//
			// 如果 没有等待的goroutine或 goroutine不处于空闲 (goroutine已经被唤醒 或 抓住了锁 或 正在饥饿)，则无需唤醒 其他任何 g
			// 在饥饿模式下，锁的所有权直接从解锁goroutine交给下一个 正在等待的goroutine (等待队列中的第一个)。
			//
			//
			// todo 注意： old&(mutexLocked|mutexWoken|mutexStarving) 中，因为在最上面已经 -mutexLocked 并且进入了 if new&mutexStarving == 0
			// todo 说明目前 只有在还有 goroutine在等待 或者 mutex 有 被唤醒的情况下才会  old&(mutexLocked|mutexWoken|mutexStarving) != 0
			//
			// todo 即：当休眠队列内的等待计数为 0  或者    是正常 但是 mutex处于被唤醒 或者 被锁定状态，退出
			// old&(mutexLocked|mutexWoken|mutexStarving) != 0     xxxx...x0xx & (0001 | 0010 | 0100) => xxxx...x0xx & 0111 != 0
			/**
			 如果没有等待的goroutine, 或者  锁不处于空闲的状态，直接返回, 而不需要在通知 其他 等待的g.
			 */
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}


			// Grab the right to wake someone.  获取 唤醒某人的权利
			//
			// 减少 等待goroutine个数，并添加 唤醒标识 todo 唤醒其中一个 等待的 g, 且等待队列数目 -1
			new = (old - 1<<mutexWaiterShift) | mutexWoken

			/**
			todo 设置新的state, 这里通过 信号量 去唤醒一个阻塞的goroutine去获取锁.
			*/
			if atomic.CompareAndSwapInt32(&m.state, old, new) {

				// 释放锁,发送释放信号 (解除 阻塞信号量)
				runtime_Semrelease(&m.sema, false, 1)
				return
			}

			// 赋值给中转变量，然后启动下一轮
			old = m.state
		}
	} else { // todo 锁处于【饥饿模式】
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		/**
		todo 饥饿模式下:
			直接将锁的拥有权传给等待队列中的第一个g.

        注意:
		此时state的mutexLocked还没有加锁，唤醒的goroutine会设置它。
        在此期间，如果有新的goroutine来请求锁， 因为mutex处于饥饿状态， mutex还是被认为处于锁状态，
        新来的goroutine不会把锁抢过去.
		 */
		runtime_Semrelease(&m.sema, true, 1)
	}
}
