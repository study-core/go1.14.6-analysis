// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// GOMAXPROCS sets the maximum number of CPUs that can be executing
// simultaneously and returns the previous setting. If n < 1, it does not
// change the current setting.
// The number of logical CPUs on the local machine can be queried with NumCPU.
// This call will go away when the scheduler improves.
//
//
// GOMAXPROCS 设置能够同时执行线程的最大 CPU 数，并返回原先的设定。
// 如果 n < 1，则他不会进行任何修改。
// 机器上的逻辑 CPU 的个数可以从 NumCPU 调用上获取。
// 该调用会在调度器进行改进后被移除
func GOMAXPROCS(n int) int {
	if GOARCH == "wasm" && n > 1 {
		n = 1 // WebAssembly has no threads yet, so only one CPU is possible.
	}

	// 当调整 P 的数量时，调度器会被锁住
	lock(&sched.lock)
	ret := int(gomaxprocs)  //
	unlock(&sched.lock)
	if n <= 0 || n == ret {
		return ret
	}

	// todo 这里就知道 执行,  runtime.GOMAXPROCS(n) 从新 设置 全局 P 数量, 是有性能停顿的   StopTheWorld  ->  修改数目 ->  StartTheWorld
	//
	stopTheWorld("GOMAXPROCS")

	// newprocs will be processed by startTheWorld
	newprocs = int32(n)

	startTheWorld()
	return ret
}

// NumCPU returns the number of logical CPUs usable by the current process.
//
// The set of available CPUs is checked by querying the operating system
// at process startup. Changes to operating system CPU allocation after
// process startup are not reflected.
func NumCPU() int {
	return int(ncpu)
}

// NumCgoCall returns the number of cgo calls made by the current process.
func NumCgoCall() int64 {
	var n int64
	for mp := (*m)(atomic.Loadp(unsafe.Pointer(&allm))); mp != nil; mp = mp.alllink {
		n += int64(mp.ncgocall)
	}
	return n
}

// NumGoroutine returns the number of goroutines that currently exist.
func NumGoroutine() int {
	return int(gcount())
}

//go:linkname debug_modinfo runtime/debug.modinfo
func debug_modinfo() string {
	return modinfo
}
