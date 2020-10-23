// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package runtime

import "unsafe"

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32)

var (
	// 全局的轮询文件描述符
	epfd int32 = -1 // epoll descriptor

	netpollBreakRd, netpollBreakWr uintptr // for netpollBreak     在 netpollinit() 被初始化, 全局用来通信的 通道变量引用
)

// 初始化网络轮询器， 保证该函数只会调用一次.
func netpollinit() {

	// 			1、调用 epollcreate1 创建一个新的 epoll 文件描述符，这个文件描述符会在整个程序的生命周期中使用.
	// 		 	2、通过 runtime.nonblockingPipe 创建一个用于通信的管道.
	// 			3、使用 epollctl 将用于读取数据的文件描述符打包成 epollevent 事件加入监听.

	epfd = epollcreate1(_EPOLL_CLOEXEC) //  创建一个新的 epoll 文件描述符，这个文件描述符会在整个程序的生命周期中使用
	if epfd < 0 {
		epfd = epollcreate(1024)
		if epfd < 0 {
			println("runtime: epollcreate failed with", -epfd)
			throw("runtime: netpollinit failed")
		}
		closeonexec(epfd)
	}
	r, w, errno := nonblockingPipe()  // 创建一个用于通信的管道    (初始化的管道为我们提供了中断多路复用等待文件描述符中事件的方法)
	if errno != 0 {
		println("runtime: pipe failed with", -errno)
		throw("runtime: pipe failed")
	}
	ev := epollevent{
		events: _EPOLLIN,
	}
	*(**uintptr)(unsafe.Pointer(&ev.data)) = &netpollBreakRd
	errno = epollctl(epfd, _EPOLL_CTL_ADD, r, &ev)  // 将用于读取数据的文件描述符打包成 epollevent 事件加入监听
	if errno != 0 {
		println("runtime: epollctl failed with", -errno)
		throw("runtime: epollctl failed")
	}

	// 赋值 用来通信的 通道变量引用
	netpollBreakRd = uintptr(r)
	netpollBreakWr = uintptr(w)
}

// 判断文件描述符是否被轮询器使用
func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(epfd) || fd == netpollBreakRd || fd == netpollBreakWr
}

// 监听文件描述符上的边缘触发事件，创建事件并加入监听
func netpollopen(fd uintptr, pd *pollDesc) int32 {
	var ev epollevent
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)  // 调用 epollctl() 向 全局的轮询文件描述符 epfd 中加入  新的轮询事件监听文件描述符的 可读 和 可写 状态
}

func netpollclose(fd uintptr) int32 {
	var ev epollevent
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)  // 从 全局的轮询文件描述符 epfd 中删除   待监听的文件描述符
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// 唤醒网络轮询器
//
// 			例如：计时器向前修改时间时会通过该函数中断网络轮询器
//
//			函数会向管道中写入数据唤醒 epoll.
//
// netpollBreak interrupts an epollwait.     netpollBreak中断 epollwait
func netpollBreak() {
	for {
		var b byte
		n := write(netpollBreakWr, unsafe.Pointer(&b), 1)  // 向管道中写入数据唤醒 epoll
		if n == 1 {
			break
		}
		if n == -_EINTR {
			continue
		}
		if n == -_EAGAIN {
			return
		}
		println("runtime: netpollBreak write failed with", -n)
		throw("runtime: netpollBreak write failed")
	}
}

//		todo 轮询网络 并返回一组已经准备就绪的 Goroutine，传入的参数会决定它的行为    (返回的 goList 等待调度器来调度)
//
//				如果参数 < 0，无限期等待文件描述符就绪.
//				如果参数 == 0，非阻塞地轮询网络.
//				如果参数 > 0，阻塞特定时间轮询网络.
//
//
// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
func netpoll(delay int64) gList {

	// Go 语言的运行时  会在 【调度】 或者 【系统监控】 中调用 runtime.netpoll() 轮询网络，该函数的执行过程可以分成以下几个部分：
	//
	//		1、根据传入的 delay 计算 epoll 系统调用需要等待的时间.
	//		2、调用 epollwait() 等待 可读 或者 可写 事件的发生.
	//		3、在循环中依次处理 epollevent 事件.
	//
	// 因为传入 delay 的单位是纳秒，下面这段代码会将纳秒转换成毫秒

	if epfd == -1 {
		return gList{}
	}

	// 根据传入的 delay 计算 epoll 系统调用需要等待的时间
	var waitms int32
	if delay < 0 {
		waitms = -1
	} else if delay == 0 {
		waitms = 0
	} else if delay < 1e6 {
		waitms = 1
	} else if delay < 1e15 {
		waitms = int32(delay / 1e6)
	} else {
		// An arbitrary cap on how long to wait for a timer.
		// 1e9 ms == ~11.5 days.
		waitms = 1e9
	}
	var events [128]epollevent
retry:
	n := epollwait(epfd, &events[0], int32(len(events)), waitms) // 调用 epollwait() 等待 可读 或者 可写 事件的发生
	// 计算了需要等待的时间之后，runtime.netpoll 会执行 epollwait() 等待文件描述符转换成可读或者可写，
	// 如果该函数返回了负值，就可能返回 空的 Goroutine 列表 或者 重新调用 epollwait() 陷入等待.
	if n < 0 {
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		if waitms > 0 {
			return gList{} // 返回 空的 Goroutine 列表
		}
		goto retry // 重新调用 epollwait() 陷入等待
	}


	// 当 epollwait() 函数 返回的值大于 0 时，就意味着被监控的文件描述符 出现了 待处理的事件

	var toRun gList

	// 在循环中依次处理 epollevent 事件
	//
	//  处理的事件总共包含两种:
	// 				一种是:		调用 runtime.netpollBreak() 函数触发的事件，该函数的作用是中断网络轮询器
	// 				另一种是:	其他文件描述符的正常读写事件，对于这些事件，我们会交给 runtime.netpollready() 处理
	for i := int32(0); i < n; i++ {
		ev := &events[i]
		if ev.events == 0 {
			continue
		}

		if *(**uintptr)(unsafe.Pointer(&ev.data)) == &netpollBreakRd {
			if ev.events != _EPOLLIN {
				println("runtime: netpoll: break fd ready for", ev.events)
				throw("runtime: netpoll: break fd ready for something unexpected")
			}
			if delay != 0 {
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the byte
				// if blocking.
				//
				// netpollBreak() 函数 可以由非阻塞 poll 测验获取.  如果阻塞则仅读取该字节   (很久 通道变量引用 `netpollBreakRd` )
				//
				var tmp [16]byte
				read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))
			}
			continue
		}

		var mode int32
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'r'
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'w'
		}
		if mode != 0 {
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data))
			pd.everr = false
			if ev.events == _EPOLLERR {
				pd.everr = true
			}
			netpollready(&toRun, pd, mode)
		}
	}
	return toRun
}
