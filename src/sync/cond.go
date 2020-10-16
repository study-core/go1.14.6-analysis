// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond implements a condition variable, a rendezvous point
// for goroutines waiting for or announcing the occurrence
// of an event.
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and
// when calling the Wait method.
//
// A Cond must not be copied after first use.   (Condition 的缩写)
type Cond struct {

	// 在 go 中, 一般习惯来讲都是值拷贝，但是这种拷贝存在的问题是锁对象的 "失效"
	//
	//
	// 想要实现NoCopy，需要自己定义一个这样的结构体实现其Lock()接口即可，结构体名字随意
	//
	// 即使包含了NoCopy的结构体，也不是真正的就不可复制了，实际上毫无影响，无论是编译，还是运行都毫不影响    todo 只有 go  vet  才能检查出来
	noCopy noCopy

	// L is held while observing or changing the condition
	//
	// 观察 或 改变状态时，需要 持有 L (锁)
	L Locker

	// 通知列表,调用Wait()方法的goroutine会被放入list中,每次唤醒,从这里取出
	// notifyList对象，维护等待唤醒的goroutine队列,使用链表实现
	// 在 sync 包中被实现， src/sync/runtime.go  (其对应的方法 其实在 sema.go 中被实现)
	notify  notifyList

	// todo 超级骚的一个操作
	// 复制检查,检查cond实例是否被复制
	// copyChecker对象，实际上是uintptr对象，保存自身对象地址
	//
	// todo 【用于检查自己是否被复制】
	checker copyChecker
}

// todo 实例化一个 Cond 实例,  需要传入一个 自己定义的 【锁】
//
// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution
// of the calling goroutine. After later resuming execution,
// Wait locks c.L before returning. Unlike in other systems,
// Wait cannot return unless awoken by Broadcast or Signal.
//
// Because c.L is not locked when Wait first resumes, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
//
//    c.L.Lock()
//    for !condition() {
//        c.Wait()
//    }
//    ... make use of condition ...
//    c.L.Unlock()
//
//
// 等待原子解锁c.L并暂停执行调用goroutine。
// 稍后恢复执行后，Wait会在返回之前锁定c.L.
// 与其他系统不同，除非被广播或信号唤醒，否则等待无法返回。
//
// 因为等待第一次恢复时c.L没有被锁定，
// 所以当Wait返回时，调用者通常不能认为条件为真。
// 相反，调用者应该循环等待：
//
//    c.L.Lock()
//    for !condition() {
//        c.Wait()
//    }
//    ... make use of condition ...
//    c.L.Unlock()
//
//
// 调用此方法会将此 goroutine 加入通知列表,并等待获取通知,调用此方法必须先Lock,不然方法里会调用Unlock(),报错
//
func (c *Cond) Wait() {

	// 检查是否被复制; 如果是就 panic
	// check检查，保证cond在第一次使用后没有被复制
	c.checker.check()

	// 将当前G 加入等待队列, 该方法在 runtime 包的 notifyListAdd 函数中实现
	// src/runtime/sema.go
	t := runtime_notifyListAdd(&c.notify)    // 其实就是一个 累加 被阻塞的G 的计数值
	c.L.Unlock()
	runtime_notifyListWait(&c.notify, t)	 // 根据 票号 t 将 当前被休眠的 g 封装成 sudog 加入 wait队列中
	c.L.Lock()
}



// 唤醒单个 等待的 goroutine
//
// Signal wakes one goroutine waiting on c, if there is any.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Signal() {
	// 检查是否被复制; 如果是就 panic
	// check检查，保证cond在第一次使用后没有被复制
	c.checker.check()

	// 唤醒下一个需要被唤醒的 G (多次调用该 方法的话，逐个从 wait 队列头往尾巴唤醒被阻塞的G)
	runtime_notifyListNotifyOne(&c.notify)
}

// 唤醒所有 等待的 goroutine
//
// Broadcast wakes all goroutines waiting on c.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Broadcast() {
	// 检查是否被复制; 如果是就 panic
	// check检查，保证cond在第一次使用后没有被复制
	c.checker.check()

	// 将当前 wait 队列中所有 被阻塞的G 唤醒
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
//
// todo 超级骚的一个操作   【用于检查自己是否被复制】
//
// `copyChecker` 保留指向自身的指针 以检测对象的复制
type copyChecker uintptr


// 用于 检查 自己是否被复制
//
//       check方法在第一次调用的时候，会将checker对象地址 赋值给checker，也就是将 自身内存地址 赋值给 自身
func (c *copyChecker) check() {
	/**
	因为 copyChecker的底层类型为 uintptr
	那么 这里的 *c其实就是 copyChecker类型本身，然后强转成uintptr
	和拿着 c 也就是copyChecker的指针去求 uintptr，理论上要想等
	即：内存地址为一样，则表示没有被复制
	 */
	// 下述做法是：
	//  todo 其实 copyChecker 中存储的对象地址就是 copyChecker 对象自身的地址
	// 先把 copyChecker 处存储的对象地址和自己通过 unsafe.Pointer求出来的对象地址作比较:
	//
	// 			如果发现不相等，那么就尝试的替换，由于使用的 old 是 0， 则表示 c 还没有开辟内存空间，也就是说，只有是首次开辟地址才会替换成功
	//
	// 			如果替换不成功，则表示 copyChecker 出所存储的地址和 unsafe 计算出来的不一致 则表示对象是被复制了
	//
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&		// 先比较

		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&  // CAS

		uintptr(*c) != uintptr(unsafe.Pointer(c)) {		// 再比较

		panic("sync.Cond is copied")
	}
}

// noCopy may be embedded into structs which must not be copied
// after the first use.
//
// noCopy 可以嵌入到第一次使用后不得复制的结构中
//
//  todo 这是一种技巧,  go vet 可以检测到
//
//		可以自己实现,  名字 可以使任意的, 不一定要交 "noCopy", 只要 有 Lock() 和 Unlock() 两个方法即可 (就是只要实现 Locker 接口即可)
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
