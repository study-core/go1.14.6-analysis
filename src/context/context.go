// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines,
// cancellation signals, and other request-scoped values across API boundaries
// and between processes.
//
// Incoming requests to a server should create a Context, and outgoing
// calls to servers should accept a Context. The chain of function
// calls between them must propagate the Context, optionally replacing
// it with a derived Context created using WithCancel, WithDeadline,
// WithTimeout, or WithValue. When a Context is canceled, all
// Contexts derived from it are also canceled.
//
// The WithCancel, WithDeadline, and WithTimeout functions take a
// Context (the parent) and return a derived Context (the child) and a
// CancelFunc. Calling the CancelFunc cancels the child and its
// children, removes the parent's reference to the child, and stops
// any associated timers. Failing to call the CancelFunc leaks the
// child and its children until the parent is canceled or the timer
// fires. The go vet tool checks that CancelFuncs are used on all
// control-flow paths.
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it. The Context should be the first
// parameter, typically named ctx:
//
// 	func DoSomething(ctx context.Context, arg Arg) error {
// 		// ... use ctx ...
// 	}
//
// Do not pass a nil Context, even if a function permits it. Pass context.TODO
// if you are unsure about which Context to use.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
//
// See https://blog.golang.org/context for example code for a server that uses
// Contexts.
package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// A Context carries a deadline, a cancellation signal, and other values across
// API boundaries.
//
// Context's methods may be called by multiple goroutines simultaneously.
type Context interface {
	// Deadline returns the time when work done on behalf of this context
	// should be canceled. Deadline returns ok==false when no deadline is
	// set. Successive calls to Deadline return the same results.
	Deadline() (deadline time.Time, ok bool)

	// Done returns a channel that's closed when work done on behalf of this
	// context should be canceled. Done may return nil if this context can
	// never be canceled. Successive calls to Done return the same value.
	// The close of the Done channel may happen asynchronously,
	// after the cancel function returns.
	//
	// WithCancel arranges for Done to be closed when cancel is called;
	// WithDeadline arranges for Done to be closed when the deadline
	// expires; WithTimeout arranges for Done to be closed when the timeout
	// elapses.
	//
	// Done is provided for use in select statements:
	//
	//  // Stream generates values with DoSomething and sends them to out
	//  // until DoSomething returns an error or ctx.Done is closed.
	//  func Stream(ctx context.Context, out chan<- Value) error {
	//  	for {
	//  		v, err := DoSomething(ctx)
	//  		if err != nil {
	//  			return err
	//  		}
	//  		select {
	//  		case <-ctx.Done():
	//  			return ctx.Err()
	//  		case out <- v:
	//  		}
	//  	}
	//  }
	//
	// See https://blog.golang.org/pipelines for more examples of how to use
	// a Done channel for cancellation.
	Done() <-chan struct{}

	// If Done is not yet closed, Err returns nil.
	// If Done is closed, Err returns a non-nil error explaining why:
	// Canceled if the context was canceled
	// or DeadlineExceeded if the context's deadline passed.
	// After Err returns a non-nil error, successive calls to Err return the same error.
	Err() error

	// Value returns the value associated with this context for key, or nil
	// if no value is associated with key. Successive calls to Value with
	// the same key returns the same result.
	//
	// Use context values only for request-scoped data that transits
	// processes and API boundaries, not for passing optional parameters to
	// functions.
	//
	// A key identifies a specific value in a Context. Functions that wish
	// to store values in Context typically allocate a key in a global
	// variable then use that key as the argument to context.WithValue and
	// Context.Value. A key can be any type that supports equality;
	// packages should define keys as an unexported type to avoid
	// collisions.
	//
	// Packages that define a Context key should provide type-safe accessors
	// for the values stored using that key:
	//
	// 	// Package user defines a User type that's stored in Contexts.
	// 	package user
	//
	// 	import "context"
	//
	// 	// User is the type of value stored in the Contexts.
	// 	type User struct {...}
	//
	// 	// key is an unexported type for keys defined in this package.
	// 	// This prevents collisions with keys defined in other packages.
	// 	type key int
	//
	// 	// userKey is the key for user.User values in Contexts. It is
	// 	// unexported; clients use user.NewContext and user.FromContext
	// 	// instead of using this key directly.
	// 	var userKey key
	//
	// 	// NewContext returns a new Context that carries value u.
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(ctx, userKey, u)
	// 	}
	//
	// 	// FromContext returns the User value stored in ctx, if any.
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	Value(key interface{}) interface{}
}

// Canceled is the error returned by Context.Err when the context is canceled.
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's
// deadline passes.
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

// An emptyCtx is never canceled, has no values, and has no deadline. It is not
// struct{}, since vars of this type must have distinct addresses.
type emptyCtx int

func (*emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (*emptyCtx) Done() <-chan struct{} {
	return nil
}

func (*emptyCtx) Err() error {
	return nil
}

func (*emptyCtx) Value(key interface{}) interface{} {
	return nil
}

func (e *emptyCtx) String() string {
	switch e {
	case background:
		return "context.Background"
	case todo:
		return "context.TODO"
	}
	return "unknown empty Context"
}

var (
	// 定义了两个  空的 ctx   (background 和  todo)
	background = new(emptyCtx)
	todo       = new(emptyCtx)
)

// Background returns a non-nil, empty Context. It is never canceled, has no
// values, and has no deadline. It is typically used by the main function,
// initialization, and tests, and as the top-level Context for incoming
// requests.
func Background() Context {
	return background
}

// TODO returns a non-nil, empty Context. Code should use context.TODO when
// it's unclear which Context to use or it is not yet available (because the
// surrounding function has not yet been extended to accept a Context
// parameter).
func TODO() Context {
	return todo
}

// A CancelFunc tells an operation to abandon its work.
// A CancelFunc does not wait for the work to stop.
// A CancelFunc may be called by multiple goroutines simultaneously.
// After the first call, subsequent calls to a CancelFunc do nothing.
type CancelFunc func()


// todo 创建 CancelContext
// WithCancel returns a copy of parent with a new Done channel. The returned
// context's Done channel is closed when the returned cancel function is called
// or when the parent context's Done channel is closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	c := newCancelCtx(parent)
	propagateCancel(parent, &c)   //  安排孩子在父母离开时被取消
	return &c, func() { c.cancel(true, Canceled) }    //  并返回一个 主动 取消 孩子的函数 (该函数中会将 孩子从 parent 中移除)
}

// newCancelCtx returns an initialized cancelCtx.
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}

// goroutines counts the number of goroutines ever created; for testing.
//
// goroutines 计算曾经创建的goroutine的数量; 供测试用.
var goroutines int32

// propagateCancel arranges for child to be canceled when parent is.
//
//  安排孩子在父母离开时被取消
func propagateCancel(parent Context, child canceler) {
	done := parent.Done()
	if done == nil {
		return // parent is never canceled
	}


	// 进来的时候 先尝试一下。 监听  parent 的chan 是否有 【取消】 信号
	select {
	case <-done:
		// parent is already canceled
		//
		// 如果  parent  已经停止了,  那么 耶取消 儿子
		child.cancel(false, parent.Err())
		return
	default:
	}

	// 返回  父级的 底层 真实 *cancelCtx  todo (可能是 parent,  也可能是 返回 爷爷)
	if p, ok := parentCancelCtx(parent); ok {
		p.mu.Lock()

		// 如果  parent 出现了错误, 则 取消掉  所有 儿子
		if p.err != nil {
			// parent has already been canceled
			child.cancel(false, p.err)
		} else {
				if p.children == nil {
				p.children = make(map[canceler]struct{})
			}

			// 将当前 儿子 追加到  parent 的 map中
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
	} else {   // todo 如果  不存在当前 parent 的 真实 ctx 呢?     （只有测试代码才会进入这里）
		atomic.AddInt32(&goroutines, +1)  // goroutines 计算曾经创建的goroutine的数量; 供测试用.
		go func() {
			select {
			case <-parent.Done():
				child.cancel(false, parent.Err())
			case <-child.Done():
			}
		}()
	}
}

// &cancelCtxKey is the key that a cancelCtx returns itself for.
var cancelCtxKey int

// parentCancelCtx returns the underlying *cancelCtx for parent.
// It does this by looking up parent.Value(&cancelCtxKey) to find
// the innermost enclosing *cancelCtx and then checking whether
// parent.Done() matches that *cancelCtx. (If not, the *cancelCtx
// has been wrapped in a custom implementation providing a
// different done channel, in which case we should not bypass it.)
//
//
// `parentCancelCtx()`  todo 返回父级的 底层 *cancelCtx
// 						通过查找 parent.Value(&cancelCtxKey) 来查找最里面的 *cancelCtx，
// 						然后检查 parent.Done() 是否与该 *cancelCtx匹配，从而实现此目的
// 						(如果没有, *cancelCtx 已包装在提供不同完成 chan 的自定义实现中，在这种情况下，我们不应绕过它)
func parentCancelCtx(parent Context) (*cancelCtx, bool) {

	//
	done := parent.Done()
	if done == closedchan || done == nil {
		return nil, false
	}

	// todo 精髓: 如果是 BackgroudCtx 那么 value 是个 nil,
	//				否咋 只有 valueCtx 是实现了 Value() 接口的, 也就是说 parent 当前是 一个 valueCtx,
	//				valueCtx的 Value() 方法里面, 会逐级向上查找 value 的
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok {
		return nil, false
	}
	p.mu.Lock()
	ok = p.done == done
	p.mu.Unlock()
	if !ok {
		return nil, false
	}
	return p, true
}

// removeChild removes a context from its parent.
//
// removeChild  从其父级中删除 当前ctx
//
// 从 parent 的 map 中移除掉 当前 孩子
func removeChild(parent Context, child canceler) {

	// 返回 parent 的真实 ctx
	p, ok := parentCancelCtx(parent)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child)  // 从 parent 的 map 中移除掉 当前 孩子
	}
	p.mu.Unlock()
}

// A canceler is a context type that can be canceled directly. The
// implementations are *cancelCtx and *timerCtx.
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}

// closedchan is a reusable closed channel.
var closedchan = make(chan struct{})

func init() {
	close(closedchan)
}

// A cancelCtx can be canceled. When canceled, it also cancels any children
// that implement canceler.
type cancelCtx struct {
	Context				// 继承了父引用

	mu       sync.Mutex            // protects following fields   保护以下字段


	done     chan struct{}         // created lazily, closed by first cancel call   	延迟创建，通过第一个取消调用关闭
	children map[canceler]struct{} // set to nil by the first cancel call				由第一个取消调用 来将这个设置为 nil
	err      error                 // set to non-nil by the first cancel call			由第一个取消调用 来将这个设置为 非零
}

func (c *cancelCtx) Value(key interface{}) interface{} {
	if key == &cancelCtxKey {
		return c
	}
	return c.Context.Value(key)
}

// cancelCtx 的 Done 实现
func (c *cancelCtx) Done() <-chan struct{} {
	c.mu.Lock()
	if c.done == nil {
		c.done = make(chan struct{})
	}
	d := c.done
	c.mu.Unlock()
	return d
}

func (c *cancelCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// cancel closes c.done, cancels each of c's children, and, if
// removeFromParent is true, removes c from its parent's children.
//
//`cancel()`关闭c.done，取消c的每个子级，如果removeFromParent为true，则从其父级的子级中删除c。
func (c *cancelCtx) cancel(removeFromParent bool, err error) {

	// err 入参 一般是 parent 的 err 入参 (能进到这里, 一般是 parent 停掉了, 所以 parent 的 err 肯定是有值的)
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return // already canceled   只有 当前  ctx 被取消过了, err 上才会有值
	}
	c.err = err   // 将父的 err 赋值给 自己

	// todo 这里是精髓:
	if c.done == nil {
		c.done = closedchan   // 直接将一个 已经 close 的 chan  赋值   (在 孩子 调用 ctx.Done() 之前收到终止信号了)
	} else {
		close(c.done)         // 否则 close 掉该 chan	(孩子已经调用了  ctx.Done() 正在阻塞中)
	}

	// 遍历 所有 小孩 (重复这个动作)
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err)
	}


	c.children = nil    // 并将小孩 设置为 nil
	c.mu.Unlock()

	if removeFromParent {
		// 从 parent 的 map 中移除掉 当前 孩子
		removeChild(c.Context, c)
	}
}


// todo 实例化一个 timeCtx
// WithDeadline returns a copy of the parent context with the deadline adjusted
// to be no later than d. If the parent's deadline is already earlier than d,
// WithDeadline(parent, d) is semantically equivalent to parent. The returned
// context's Done channel is closed when the deadline expires, when the returned
// cancel function is called, or when the parent context's Done channel is
// closed, whichever happens first.
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete.
//
//
//	如果deadline到来之前手动关闭， 则关闭原因与cancelCtx显示一致；
//	如果deadline到来时自动关闭， 则原因为："context deadline exceeded"
//
//
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {

	// 先看看 parent 的 timer 到期没
	//
	// parent 的已经到期, 那么 直接  【取消】 parent
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		// The current deadline is already sooner than the new one.
		return WithCancel(parent)
	}

	// todo 正常流程

	// 初始化一个timerCtx实例
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	// 将timerCtx实例添加到其父节点的children中(如果父节点也可以被cancel的话)
	propagateCancel(parent, c)

	// todo  再一次 校验 设定的时间
	dur := time.Until(d)
	if dur <= 0 {
		c.cancel(true, DeadlineExceeded) // deadline has already passed
		return c, func() { c.cancel(false, Canceled) }
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err == nil {

		// todo 核心流程
		//
		// 启动定时器， 定时器到期后会自动cancel本context
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded)
		})
	}

	// 将 ctx  和 【取消】 函数返回, 用于手动关闭
	return c, func() { c.cancel(true, Canceled) }
}

// TODO timerCtx 的实现
// A timerCtx carries a timer and a deadline. It embeds a cancelCtx to
// implement Done and Err. It implements cancel by stopping its timer then
// delegating to cancelCtx.cancel.
type timerCtx struct {
	cancelCtx			// todo 继承了 cancelCtx ,  并加了一些 timer 相关


	timer *time.Timer // Under cancelCtx.mu.   timer就是一个触发自动 cancel 的定时器
	deadline time.Time    // 用于标示自动cancel的最终时间，
}

// 返回timerCtx.deadline
func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

// 这个很重要
//
// cancel() 方法基本继承cancelCtx， 只需要额外把timer关闭
func (c *timerCtx) cancel(removeFromParent bool, err error) {
	c.cancelCtx.cancel(false, err)
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	c.mu.Lock()

	// 关闭 定时器
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// todo 封装了 WithDeadline()
//
// WithTimeout returns WithDeadline(parent, time.Now().Add(timeout)).
//
// Canceling this context releases resources associated with it, so code should
// call cancel as soon as the operations running in this Context complete:
//
// 	func slowOperationWithTimeout(ctx context.Context) (Result, error) {
// 		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
// 		defer cancel()  // releases resources if slowOperation completes before timeout elapses
// 		return slowOperation(ctx)
// 	}
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}


// todo 实例化  valueCtx
// WithValue returns a copy of parent in which the value associated with key is
// val.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The provided key must be comparable and should not be of type
// string or any other built-in type to avoid collisions between
// packages using context. Users of WithValue should define their own
// types for keys. To avoid allocating when assigning to an
// interface{}, context keys often have concrete type
// struct{}. Alternatively, exported context key variables' static
// type should be a pointer or interface.
func WithValue(parent Context, key, val interface{}) Context {
	if key == nil {
		panic("nil key")
	}
	if !reflectlite.TypeOf(key).Comparable() {  // key 的类型  必须可以比较哦
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

// todo valueCtx 的实现
// A valueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded Context.
type valueCtx struct {
	Context

	// 只是在 Context 基础上增加了一个key-value对， 用于在各级协程间传递一些数据
	//
	// 由于valueCtx既不需要cancel， 也不需要deadline， 那么只需要实现Value()接口即可
	key, val interface{}
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
func stringify(v interface{}) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	}
	return "<not Stringer>"
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(type " +
		reflectlite.TypeOf(c.key).String() +
		", val " + stringify(c.val) + ")"
}

// 由于valueCtx既不需要cancel， 也不需要deadline， 那么只需要实现Value()接口即可
func (c *valueCtx) Value(key interface{}) interface{} {

	// 入参的key 和 存储的key 一致时,  返回 value
	if c.key == key {
		return c.val
	}

	// 当 找不到时, 往 继承的 父节点查找
	return c.Context.Value(key)
}
