// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Should be a built-in for unsafe.Pointer?
//go:nosplit
func add(p unsafe.Pointer, x uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(p) + x)
}

// getg returns the pointer to the current g.
// The compiler rewrites calls to this function into instructions
// that fetch the g directly (from TLS or from the dedicated register).
//
// todo 获取当前G
// getg() 返回指向当前g的指针
// 编译器将此函数的调用 重写为直接获取g的指令（从TLS或专用寄存器）。
//
func getg() *g

// mcall switches from the g to the g0 stack and invokes fn(g),
// where g is the goroutine that made the call.
// mcall saves g's current PC/SP in g->sched so that it can be restored later.
// It is up to fn to arrange for that later execution, typically by recording
// g in a data structure, causing something to call ready(g) later.
// mcall returns to the original goroutine g later, when g has been rescheduled.
// fn must not return at all; typically it ends by calling schedule, to let the m
// run other goroutines.
//
// mcall can only be called from g stacks (not g0, not gsignal).
//
// This must NOT be go:noescape: if fn is a stack-allocated closure,
// fn puts g on a run queue, and g executes before fn returns, the
// closure will be invalidated while it is still executing.
//
// TODO 该函数操作如下:
//  		设置g.sched.pc等于当前的返回地址
//  		设置g.sched.sp等于寄存器rsp的值
//  		设置g.sched.g等于当前的g
//  		设置g.sched.bp等于寄存器rbp的值
//  		切换TLS中当前的g等于m.g0
//  		设置寄存器rsp等于g0.sched.sp, 使用g0的栈空间
//  		设置第一个参数为原来的g
//  		设置rdx寄存器为指向函数地址的指针(上下文)
//  		调用指定的函数, 不会返回
//
//
//  作用就是将当前线程的执行栈先切换到g0上
//  然后将当前的goroutine（即执行完的goroutine对应的G实例）当成参数传给fn，
//	在g0的栈上继续执行fn。等于换了个SP指针，然后继续执行goexit0.
//
func mcall(fn func(*g))

// systemstack runs fn on a system stack.
// If systemstack is called from the per-OS-thread (g0) stack, or
// if systemstack is called from the signal handling (gsignal) stack,
// systemstack calls fn directly and returns.
// Otherwise, systemstack is being called from the limited stack
// of an ordinary goroutine. In this case, systemstack switches
// to the per-OS-thread stack, calls fn, and switches back.
// It is common to use a func literal as the argument, in order
// to share inputs and outputs with the code around the call
// to system stack:
//
//	... set up y ...
//	systemstack(func() {
//		x = bigcall(y)
//	})
//	... use x ...
//
/**
	todo `systemstack()`  在系统堆栈上运行fn

			如果从每个OS线程（g0）堆栈中调用 systemstack，或者从信号处理（gsignal）堆栈中调用systemstack，则systemstack直接调用fn并返回

			否则，将从普通goroutine的有限堆栈中调用systemstack。 在这种情况下，系统堆栈切换到每个OS线程堆栈，调用fn，然后切回。

			通常使用func文字作为参数，以便与调用系统堆栈周围的代码共享输入和输出：

			 ...设置y ...
			 systemstack（func（）{
			 x = bigcall（y）
			}）
			 ...使用x ...

 */
//
//go:noescape
func systemstack(fn func())

var badsystemstackMsg = "fatal: systemstack called from unexpected goroutine"

//go:nosplit
//go:nowritebarrierrec
func badsystemstack() {
	sp := stringStructOf(&badsystemstackMsg)
	write(2, sp.str, int32(sp.len))
}

// memclrNoHeapPointers clears n bytes starting at ptr.
//
// Usually you should use typedmemclr. memclrNoHeapPointers should be
// used only when the caller knows that *ptr contains no heap pointers
// because either:
//
// *ptr is initialized memory and its type is pointer-free, or
//
// *ptr is uninitialized memory (e.g., memory that's being reused
// for a new allocation) and hence contains only "junk".
//
// The (CPU-specific) implementations of this function are in memclr_*.s.
//go:noescape
func memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr)

//go:linkname reflect_memclrNoHeapPointers reflect.memclrNoHeapPointers
func reflect_memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr) {
	memclrNoHeapPointers(ptr, n)
}

// memmove copies n bytes from "from" to "to".
//
// memmove ensures that any pointer in "from" is written to "to" with
// an indivisible write, so that racy reads cannot observe a
// half-written pointer. This is necessary to prevent the garbage
// collector from observing invalid pointers, and differs from memmove
// in unmanaged languages. However, memmove is only required to do
// this if "from" and "to" may contain pointers, which can only be the
// case if "from", "to", and "n" are all be word-aligned.
//
// Implementations are in memmove_*.s.
//
//go:noescape
func memmove(to, from unsafe.Pointer, n uintptr)  // 汇编实现

//go:linkname reflect_memmove reflect.memmove
func reflect_memmove(to, from unsafe.Pointer, n uintptr) {
	memmove(to, from, n)
}

// exported value for testing
var hashLoad = float32(loadFactorNum) / float32(loadFactorDen)

// todo 随机函数的底层实现
//go:nosplit
func fastrand() uint32 {

	// 获取当前G 对应的 M
	mp := getg().m
	// Implement xorshift64+: 2 32-bit xorshift sequences added together.
	// Shift triplet [17,7,16] was calculated as indicated in Marsaglia's
	// Xorshift paper: https://www.jstatsoft.org/article/view/v008i14/xorshift.pdf
	// This generator passes the SmallCrush suite, part of TestU01 framework:
	// http://simul.iro.umontreal.ca/testu01/tu01.html
	s1, s0 := mp.fastrand[0], mp.fastrand[1]
	s1 ^= s1 << 17
	s1 = s1 ^ s0 ^ s1>>7 ^ s0>>16
	mp.fastrand[0], mp.fastrand[1] = s0, s1
	return s0 + s1
}

//go:nosplit
func fastrandn(n uint32) uint32 {
	// This is similar to fastrand() % n, but faster.
	// See https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
	return uint32(uint64(fastrand()) * uint64(n) >> 32)
}

//go:linkname sync_fastrand sync.fastrand
func sync_fastrand() uint32 { return fastrand() }

// in asm_*.s
//go:noescape
func memequal(a, b unsafe.Pointer, size uintptr) bool

// noescape 从转义分析中隐藏一个指针. noescape是标识函数，但是转义分析不认为输出取决于输入. noescape已内联，当前可编译为零指令.
//
// 小心使用!
//
// noescape hides a pointer from escape analysis.  noescape is
// the identity function but escape analysis doesn't think the
// output depends on the input.  noescape is inlined and currently
// compiles down to zero instructions.
// USE CAREFULLY!
//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func cgocallback(fn, frame unsafe.Pointer, framesize, ctxt uintptr)
func gogo(buf *gobuf)  // 汇编实现
func gosave(buf *gobuf)

//go:noescape
func jmpdefer(fv *funcval, argp uintptr)  // 汇编实现
func asminit()
func setg(gg *g)
func breakpoint()

// reflectcall calls fn with a copy of the n argument bytes pointed at by arg.
// After fn returns, reflectcall copies n-retoffset result bytes
// back into arg+retoffset before returning. If copying result bytes back,
// the caller should pass the argument frame type as argtype, so that
// call can execute appropriate write barriers during the copy.
// Package reflect passes a frame type. In package runtime, there is only
// one call that copies results back, in cgocallbackg1, and it does NOT pass a
// frame type, meaning there are no write barriers invoked. See that call
// site for justification.
//
// Package reflect accesses this symbol through a linkname.
func reflectcall(argtype *_type, fn, arg unsafe.Pointer, argsize uint32, retoffset uint32)  // 跨平台汇编实现,  延迟调用

// 跨平台汇编实现, 当前 g 的自旋
func procyield(cycles uint32)

type neverCallThisFunction struct{}

// goexit is the return stub at the top of every goroutine call stack.
// Each goroutine stack is constructed as if goexit called the
// goroutine's entry point function, so that when the entry point
// function returns, it will return to goexit, which will call goexit1
// to perform the actual exit.
//
// This function must never be called directly. Call goexit1 instead.
// gentraceback assumes that goexit terminates the stack. A direct
// call on the stack will cause gentraceback to stop walking the stack
// prematurely and if there is leftover state it may panic.
func goexit(neverCallThisFunction)

// Not all cgocallback_gofunc frames are actually cgocallback_gofunc,
// so not all have these arguments. Mark them uintptr so that the GC
// does not misinterpret memory when the arguments are not present.
// cgocallback_gofunc is not called from go, only from cgocallback,
// so the arguments will be found via cgocallback's pointer-declared arguments.
// See the assembly implementations for more details.
func cgocallback_gofunc(fv, frame, framesize, ctxt uintptr)

// publicationBarrier performs a store/store barrier (a "publication"
// or "export" barrier). Some form of synchronization is required
// between initializing an object and making that object accessible to
// another processor. Without synchronization, the initialization
// writes and the "publication" write may be reordered, allowing the
// other processor to follow the pointer and observe an uninitialized
// object. In general, higher-level synchronization should be used,
// such as locking or an atomic pointer write. publicationBarrier is
// for when those aren't an option, such as in the implementation of
// the memory manager.
//
// There's no corresponding barrier for the read side because the read
// side naturally has a data dependency order. All architectures that
// Go supports or seems likely to ever support automatically enforce
// data dependency ordering.
func publicationBarrier()

// getcallerpc returns the program counter (PC) of its caller's caller.
// getcallersp returns the stack pointer (SP) of its caller's caller.
// The implementation may be a compiler intrinsic; there is not
// necessarily code implementing this on every platform.
//
// For example:
//
//	func f(arg1, arg2, arg3 int) {
//		pc := getcallerpc()
//		sp := getcallersp()
//	}
//
// These two lines find the PC and SP immediately following
// the call to f (where f will return).
//
// The call to getcallerpc and getcallersp must be done in the
// frame being asked about.
//
// The result of getcallersp is correct at the time of the return,
// but it may be invalidated by any subsequent call to a function
// that might relocate the stack in order to grow or shrink it.
// A general rule is that the result of getcallersp should be used
// immediately and can only be passed to nosplit functions.


//go:noescape
func getcallerpc() uintptr   // 返回 调用者的 pc 寄存器

//go:noescape
func getcallersp() uintptr // implemented as an intrinsic on all platforms   返回 调用者的 sp 寄存器

// getclosureptr returns the pointer to the current closure.
// getclosureptr can only be used in an assignment statement
// at the entry of a function. Moreover, go:nosplit directive
// must be specified at the declaration of caller function,
// so that the function prolog does not clobber the closure register.
// for example:
//
//	//go:nosplit
//	func f(arg1, arg2, arg3 int) {
//		dx := getclosureptr()
//	}
//
// The compiler rewrites calls to this function into instructions that fetch the
// pointer from a well-known register (DX on x86 architecture, etc.) directly.
func getclosureptr() uintptr   // 获取 闭包 指针 ??

//go:noescape
func asmcgocall(fn, arg unsafe.Pointer) int32

func morestack()  // 汇编实现  todo (栈扩张底层函数) 函数会保存G的状态到g.sched, 切换到g0和g0的栈空间, 然后调用 newstack() 函数
func morestack_noctxt() // 汇编实现  todo (栈扩张的入口函数)   morestack_noctxt()函数 清空rdx寄存器并调用 morestack()函数
func rt0_go()  // todo 这个是 `go程序入口点`     (有很多种平台的 汇编实现)

// return0 is a stub used to return 0 from deferproc.
// It is called at the very end of deferproc to signal
// the calling Go function that it should not jump
// to deferreturn.
// in asm_*.s
//
//
//
// todo 在 deferproc() 的最后被调用，以向调用 Go 函数 发出信号，告知它不应跳转到 deferreturn() .  在 asm_*.s 中实现     (其实在 deferStack() 中最后也被调用)
func return0()

// in asm_*.s
// not called directly; definitions here supply type information for traceback.
func call32(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call64(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call128(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call256(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call512(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1024(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2048(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4096(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8192(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16384(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call32768(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call65536(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call131072(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call262144(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call524288(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1048576(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2097152(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4194304(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8388608(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16777216(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call33554432(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call67108864(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call134217728(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call268435456(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call536870912(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1073741824(typ, fn, arg unsafe.Pointer, n, retoffset uint32)

func systemstack_switch()

// alignUp rounds n up to a multiple of a. a must be a power of 2.
func alignUp(n, a uintptr) uintptr {
	return (n + a - 1) &^ (a - 1)
}

// alignDown rounds n down to a multiple of a. a must be a power of 2.
func alignDown(n, a uintptr) uintptr {
	return n &^ (a - 1)
}

// divRoundUp returns ceil(n / a).
func divRoundUp(n, a uintptr) uintptr {
	// a is generally a power of two. This will get inlined and
	// the compiler will optimize the division.
	return (n + a - 1) / a
}

// checkASM reports whether assembly runtime checks have passed.
func checkASM() bool

func memequal_varlen(a, b unsafe.Pointer) bool

// bool2int returns 0 if x is false or 1 if x is true.
func bool2int(x bool) int {
	// Avoid branches. In the SSA compiler, this compiles to
	// exactly what you would want it to.
	return int(uint8(*(*uint8)(unsafe.Pointer(&x))))
}

// abort crashes the runtime in situations where even throw might not
// work. In general it should do something a debugger will recognize
// (e.g., an INT3 on x86). A crash in abort is recognized by the
// signal handler, which will attempt to tear down the runtime
// immediately.
func abort()

// Called from compiled code; declared for vet; do NOT call from Go.
func gcWriteBarrier()
func duffzero()
func duffcopy()

// Called from linker-generated .initarray; declared for go vet; do NOT call from Go.
func addmoduledata()
