// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
//
//  对象池 pool 实现
//
// 保存和复用临时对象，减少内存分配，降低GC压力
type Pool struct {

	// 和 WaitGroup 及  Cond 中的用法一样
	noCopy noCopy

	/** local 和 localSize 维护一个动态 poolLocal 数组 */

	// 每个固定大小的池， 真实类型是 [P]poolLocal
	// 其实就是一个[P]poolLocal 的指针地址
	//
	// local 数组的头指针     todo 元素就是  poolLocal 实例
	//
	//     local 数组的 下标是 PID, 预示着 不同的 PID 操作 当前 pool.local 数组时, 放置的 poolLocal 对象是 在各自的 PID 下标处的
	//		咋一看, 就是 不同的 P 操作者 一个 pool.local数组, 然后里面的 小pool (就是 poolLocal) 才是真正放置 obj 链表的地方
	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	// local 数组的大小
	localSize uintptr        // size of the local array

	// 上一个周期的 local 数组
	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	//
	// `New()`  可以选择指定一个函数， 给 pool.Get() 使用的, 用来生成一个 新对象
	//
	//  在做 pool.Get() 时, 不要更改 New() 的函数指针
	New func() interface{}
}

// Local per-P Pool appendix.    每一个  pool 的 Local数组的实现  附录
type poolLocalInternal struct {
	private interface{} // Can be used only by the respective P.					只能由相应的P使用     (私有缓冲区)  todo 只放一个 obj
	shared  poolChain   // Local P can pushHead/popHead; any P can popTail.			本地P 可以 pushHead() / popHead(); 任何P 都可以 popTail()     (公共缓冲区)    todo 链表, 放多个 obj
}



/**
【注意】
因为 poolLocal 中的对象可能会被其他P偷走，

private 域 保证这个P不会被偷光，至少保留一个对象供自己用

因为如果这个P只剩一个对象，被偷走了，
那么当它本身需要对象时又要从别的P偷回来，造成了不必要的开销
 */
type poolLocal struct {
	poolLocalInternal // 每一个  pool 的 Local数组的实现  附录

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .    防止在 128 mod（缓存行大小）= 0 的广泛平台上进行虚假共享
	//
	//
	/**
	cache 使用中常见的一个问题是 false sharing （伪共享）

	当不同的线程同时读写同一 cache line 上不同数据时就可能发生false sharing

	false sharing会导致多核处理器上严重的系统性能下降
	 */
	// 字节对齐，避免 false sharing （伪共享）
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte  // 仅仅用来做对齐用的 ?  高级!!
}

// from runtime
//
// 在 runtime 包中实现 /src/runtime/stubs.go 的 sync_fastrand()
func fastrand() uint32   // 随机函数

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}



/**
总的来说，sync.Pool的定位不是做类似 连接池 的东西，它的用途仅仅是 【增加对象重用的几率】,【减少gc的负担】， todo 而开销方面也不是很便宜的
*/


// Put adds x to the pool.
//
// 归还对象的过程：
// 		1）固定到 (pin到) 某个P，如果 私有对象为空则放到私有对象
// 		2）否则 加入到该P子池的共享列表中（需要加锁）
//
// 可以看到一次put操作最少0次加锁，最多1 次加锁
//
func (p *Pool) Put(x interface{}) {

	// 如果放入的值为空，直接 return.
	//  检查当前goroutine的是否设置对象池私有值，
	// 如果没有则将x赋值给其私有成员，并将x设置为nil。
	// 如果当前goroutine私有值已经被设置，那么将该值追加到共享列表。
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}

	// 先获得 当前pool.local数组中的 特定位置的 poolLocal 实例
	l, _ := p.pin()
	if l.private == nil {
		l.private = x     	// 将 x 变量指向的 obj 引用赋给 这个 poolLocal.private 变量
		x = nil				// 切断 x 变量的引用指向
	}

	// 如果 没有将 x 指向的 obj 放置到 poolLocal.private 中时,   我们就将它放到 poolLocal.shared中
	if x != nil {
		l.shared.pushHead(x)
	}
	runtime_procUnpin()  // 取消禁用抢占 (取消被固定住的 G 和 M)
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
//
// 获取对象过程是：
// 		1）固定到某个pool，尝试从 私有对象获取，如果 私有对象非空则返回该对象，并把私有对象置空
// 		2）如果私有对象是空的时候，就去当前子池的共享列表获取（需要加锁）
// 		3）如果当前子池的共享列表也是空的，那么就尝试去 其他pool的子池的共享列表偷取一个（需要加锁）,并删除其他P的共享池中的该值(p.getSlow())；
// 		4）如果其他子池都是空的，最后就用用户指定的 New() 函数产生一个新的对象返回。todo 注意这个分配的值不会被放入池中
//
// 可以看到一次get操作最少0次加锁，最大N（N等于MAXPROCS）次加锁
//
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}

	// 固定住 当前 G 和 M 并返回  poolLoacl 和 PID
	l, pid := p.pin()

	// 先拿 自己的 private 中的 obj
	x := l.private
	l.private = nil
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		x, _ = l.shared.popHead()   // 再拿 自己 shared 链表中的 一个 obj
		if x == nil {
			x = p.getSlow(pid)     // 尝试 去 其他 pool 实例中 偷出 一个  obj
		}
	}

	// 取消 被禁用的 G 和 M
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}

	// 还是没有 获取一个 obj ,  则 临时创建一个 返回出去
	if x == nil && p.New != nil {
		x = p.New()
	}
	return x
}

func (p *Pool) getSlow(pid int) interface{} {
	// See the comment in pin regarding ordering of the loads.
	size := atomic.LoadUintptr(&p.localSize) // load-acquire
	locals := p.local                        // load-consume
	// Try to steal one element from other procs.
	//
	//  todo 经过 N 次尝试去 其他的 某些 pool 中获取一个 obj
	//
	//   其实也不是直接去 其他  P 中获取, 还是这个 pool 只不过每个 pool 中的 local 都会根据 PID 将不同 obj 隐性的 绑定到各个 P上
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
func (p *Pool) pin() (*poolLocal, int) {

	/***
	pin() 首先会调用运行时实现获得当前 P 的 id，
	将 P 设置为禁止抢占。然后检查 pid 与 p.localSize 的值
	来确保从 p.local 中取值不会发生越界。如果不会发生，
	则调用 indexLocal() 完成取值。否则还需要继续调用 pinSlow() 。
	 */

	// 固定当前 G 和 M, 使之相关资源不会被 GC 回收
	pid := runtime_procPin()

	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	//
	//
	// 在pinSlow中，我们先存储到local，然后再存储到localSize，这里我们以相反的顺序加载。
	// todo 由于我们 已禁用抢占，因此GC不能在两者之间发生。
	// 因此，在这里我们必须观察local至少与localSize一样大。
	// 我们可以观察到一个新的/更大的 local，这很好（我们必须观察其零初始化度）。
	s := atomic.LoadUintptr(&p.localSize) // load-acquire
	l := p.local                          // load-consume

	// 因为可能存在动态的 P（运行时调整 P 的个数）procresize/GOMAXPROCS
	// 如果 P.id 没有越界，则直接返回   PID
	//
	//
	// 具体的逻辑就是首先拿到当前的pid, 然后以pid作为index找到local中的poolLocal，
	// 但是如果pid大于了localsize， 说明当前线程的poollocal不存在,就会新创建一个poolLocal
	if uintptr(pid) < s {
		// 说明空间已分配好，直接返回
		return indexLocal(l, pid), pid
	}

	// 没有结果时，涉及全局加锁
	// 例如重新分配数组内存，添加到全局列表
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {

	/**
	因为需要对全局进行加锁，pinSlow() 会首先取消 P 的不可抢占，然后使用 allPoolsMu 进行加锁
	 */

	// 在互斥锁下重试。
	// 固定时无法锁定互斥锁。
	// 这时取消 P 的禁止抢占，因为使用 mutex 时候 P 必须可抢占

	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	//
	// 在互斥锁下重试
	//  pin 后无法锁定互斥锁， 所以需要 unpin
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()

	// 当锁住后，再次固定 P 取其 id
	pid := runtime_procPin()


	// poolCleanup won't be called while we are pinned.   固定后，不会调用 poolCleanup()
	//
	//
	// 并再次检查是否符合条件，因为可能中途已被其他线程调用
	// 当再次固定 P 时 poolCleanup 不会被调用
	s := p.localSize
	l := p.local

	// 操作 和 pin() 中一个样
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}

	// 否则,
	// 将其添加到 allPools， GC 从这里获取所有 Pool 实例
	if p.local == nil {
		allPools = append(allPools, p)     // todo 只有首次实例化的 pool 才会被放置到  全局的pool 池总, 首次实例化的 pool的 local 数组就是 nil
	}

	// todo 如果 GOMAXPROCS 在 GC 之间更改，我们将重新分配该数组，并丢失旧的数组
	//
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	//
	// 根据 P 数量创建 slice，如果 GOMAXPROCS 在 GC 间发生变化
	// 我们重新分配此数组并丢弃旧的
	size := runtime.GOMAXPROCS(0)   // 可以知道 local数组中可以 放置的 obj 的个数 以  目前系统做多可以有几个 P 而定 todo (但是 为什么 修改为 0 ？？？？)
	local := make([]poolLocal, size)

	// 将底层数组起始指针保存到 p.local，并设置 p.localSize
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release      将 新建的 local 数组存到  pool.local 变量
	atomic.StoreUintptr(&p.localSize, uintptr(size))         // store-release

	// 返回所需的 pollLocal
	return &local[pid], pid
}


// todo 实现 pool 的清理    (GC 发生时, 会调用这个)
// todo		当 stop the world  (STW) 来临，在 GC 之前会调用该函数
func poolCleanup() {

	/***
	可以看到pool包在init的时候注册了一个poolCleanup函数，
	它会清除所有的pool里面的所有缓存的对象，
	该函数注册进去之后会在每次gc之前都会调用，
	因此sync.Pool缓存的期限只是两次gc之间这段时间。
	例如我们把上面的例子改成下面这样之后，输出的结果将是0 0。

		a := p.Get().(int)
    	p.Put(1)
    	runtime.GC()
    	b := p.Get().(int)
    	fmt.Println(a, b)

	正因gc的时候会清掉缓存对象，也不用担心pool会无限增大的问题。
	 */

	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.    清空掉 就有的  oldPools 数组中所有的 pool实例 中的  victim 数组
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.  将 allPools 中的 所有的 pool 实例的  local 数组变为  victim 数组  (老龄化它们, 使之 下次 GC 到来时, 回收它们)
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	oldPools, allPools = allPools, nil    //  调换指针, 且清空 allPools
}

var (
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	//
	// allPools 是具有 非空主缓存的 pool 的集合
	// 			1）allPoolsMu 和固定
	//	或者
	// 			2）STW保护
	allPools []*Pool    // todo  这里管理着 全部  pool    （这里面是放着各种各样 Obj 的 pool 哦）

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.    oldPools是可能具有 非空victim (受害者)  缓存的一组池.  受STW保护
	oldPools []*Pool
)

func init() {
	/**
	可以看到在init的时候注册了一个PoolCleanup函数，
	他会清除掉sync.Pool中的所有的缓存的对象，
	这个注册函数会在每次GC的时候运行，
	 todo 所以【sync.Pool中的值只在两次GC中间的时段有效】。
	通过以上的解读，我们可以看到，Get() 方法并不会对获取到的对象值做任何的保证，

	因为放入本地池中的值有可能会在任何时候被删除，

	但是不通知调用者。放入共享池中的值也有可能被其他的goroutine偷走。

	 todo 所以对象池比较适合用来存储一些临时切状态无关的数据，
	但是不适合用来存储数据库连接的实例，以及 net conn 等，
	因为存入对象池重的值有可能会在垃圾回收时被删除掉，
	这违反了数据库连接池建立的初衷。
	 */

	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {

	// l 当前 pool 的 local数值的 地址
	// i 是 某个 PID
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// 注册 清除 sync.pool 实例回调函数 的方法  (主要给 GC 用)
//
// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())  // 在 mgc.go 的 runtime.sync_runtime_registerPoolCleanup() 才是真正的实现

// 固定当前 G 和 M, 使之相关资源不会被 GC 回收
func runtime_procPin() int   	// todo  proc.go 中 runtime.sync_runtime_procPin() 才是真正的实现函数
// G 和 M 取消固定 (资源 在GC时 可以被回收)
func runtime_procUnpin()		// todo  proc.go 中 runtime.sync_runtime_procUnpin() 才是真正的实现函数
