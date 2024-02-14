// Package memoize provides a duplicate function call suppression and caching mechanism.
// It is similar to [golang.org/x/sync/singleflight], but it caches the results.
package memoize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// for testing
var nowFunc = time.Now

// errGoexit indicates the runtime.Goexit was called in
// the user given function.
var errGoexit = errors.New("runtime.Goexit was called")

// A panicError is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type panicError struct {
	value interface{}
	stack []byte
}

// Error implements error interface.
func (p *panicError) Error() string {
	return fmt.Sprintf("%v\n\n%s", p.value, p.stack)
}

func (p *panicError) Unwrap() error {
	err, ok := p.value.(error)
	if !ok {
		return nil
	}

	return err
}

func newPanicError(v interface{}) error {
	stack := debug.Stack()

	// The first line of the stack trace is of the form "goroutine N [status]:"
	// but by the time the panic reaches Do the goroutine may no longer exist
	// and its status will have changed. Trim out the misleading line.
	if line := bytes.IndexByte(stack[:], '\n'); line >= 0 {
		stack = stack[line+1:]
	}
	return &panicError{value: v, stack: stack}
}

// Group represents a class of work and forms a namespace in which units of work can be executed with duplicate suppression.
type Group[K comparable, V any] struct {
	m sync.Map // K -> *entry
}

type entry[V any] struct {
	mu        sync.RWMutex
	val       V
	expiresAt time.Time
	call      *call[V]
}

type call[V any] struct {
	ctx    context.Context
	cancel context.CancelFunc
	runs   int
	chans  []chan<- Result[V]
}

type Result[V any] struct {
	Val       V
	ExpiresAt time.Time
	Err       error
}

// Do executes and memoizes the results of the given function, making sure that only one execution is in-flight for a given key at a time.
// If a duplicate comes in, the duplicate caller waits for the original to complete and receives the same results.
// The memoized results are available until expiredAt.
func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(ctx context.Context, key K) (val V, expiresAt time.Time, err error)) (V, time.Time, error) {
	now := nowFunc()

	actual, loaded := g.m.LoadOrStore(key, new(entry[V]))
	e := actual.(*entry[V])
	if loaded {
		e.mu.RLock()
		val, expiresAt := e.val, e.expiresAt
		e.mu.RUnlock()
		if now.Before(expiresAt) {
			// the cache is available.
			return val, expiresAt, nil
		}
	}

	// the cache is expired or unavailable.
	e.mu.Lock()
	if !e.expiresAt.IsZero() && now.Before(e.expiresAt) {
		// the cache is available.
		val, expiresAt := e.val, e.expiresAt
		e.mu.Unlock()
		return val, expiresAt, nil
	}
	c := e.call
	if c == nil {
		// it is the first call.
		c = new(call[V])
		c.ctx, c.cancel = context.WithCancel(withoutCancel(ctx))
		e.call = c
		go do(g, e, c, key, fn)
	}
	ch := make(chan Result[V], 1)
	c.chans = append(c.chans, ch)
	c.runs++
	e.mu.Unlock()

	select {
	case ret := <-ch:
		if e, ok := ret.Err.(*panicError); ok {
			panic(e)
		}
		if ret.Err == errGoexit {
			runtime.Goexit()
		}
		return ret.Val, ret.ExpiresAt, ret.Err
	case <-ctx.Done():
		e.mu.Lock()
		c.runs--
		if c.runs == 0 {
			c.cancel()
			// to avoid adding new channels to c.chans
			if e.call == c {
				e.call = nil
			}
		}
		e.mu.Unlock()
		var zero V
		return zero, time.Time{}, ctx.Err()
	}
}

// DoChan is like Do but returns a channel that will receive the
// results when they are ready.
//
// The returned channel will not be closed.
func (g *Group[K, V]) DoChan(ctx context.Context, key K, fn func(ctx context.Context, key K) (val V, expiresAt time.Time, err error)) <-chan Result[V] {
	now := nowFunc()

	ch := make(chan Result[V], 1)
	actual, loaded := g.m.LoadOrStore(key, new(entry[V]))
	e := actual.(*entry[V])
	if loaded {
		e.mu.RLock()
		val, expiresAt := e.val, e.expiresAt
		e.mu.RUnlock()
		if now.Before(expiresAt) {
			// the cache is available.
			ch <- Result[V]{
				Val:       val,
				ExpiresAt: expiresAt,
			}
			return ch
		}
	}

	// the cache is expired or unavailable.
	e.mu.Lock()
	if !e.expiresAt.IsZero() && now.Before(e.expiresAt) {
		// the cache is available.
		val, expiresAt := e.val, e.expiresAt
		e.mu.Unlock()
		ch <- Result[V]{
			Val:       val,
			ExpiresAt: expiresAt,
		}
		return ch
	}
	c := e.call
	if c == nil {
		// it is the first call.
		c = new(call[V])
		c.ctx, c.cancel = context.WithCancel(withoutCancel(ctx))
		e.call = c
		go do(g, e, c, key, fn)
	}
	c.chans = append(c.chans, ch)
	c.runs++
	e.mu.Unlock()
	return ch
}

func do[K comparable, V any](g *Group[K, V], e *entry[V], c *call[V], key K, fn func(ctx context.Context, key K) (V, time.Time, error)) {
	var ret Result[V]

	normalReturn := false
	recovered := false

	// use double-defer to distinguish panic from runtime.Goexit,
	// more details see https://golang.org/cl/134395
	defer func() {
		// the given function invoked runtime.Goexit
		if !normalReturn && !recovered {
			ret.Err = errGoexit
		}

		e.mu.Lock()
		if e.call == c {
			// save to the cache
			if ret.Err == nil {
				e.val = ret.Val
				e.expiresAt = ret.ExpiresAt
			}
			// to avoid adding new channels to c.chans
			e.call = nil
		}
		chans := c.chans
		e.mu.Unlock()

		// Normal return
		// notify the result to the callers
		for _, ch := range chans {
			ch <- ret
		}
	}()

	func() {
		defer func() {
			c.cancel()
			if !normalReturn {
				// Ideally, we would wait to take a stack trace until we've determined
				// whether this is a panic or a runtime.Goexit.
				//
				// Unfortunately, the only way we can distinguish the two is to see
				// whether the recover stopped the goroutine from terminating, and by
				// the time we know that, the part of the stack trace relevant to the
				// panic has been discarded.
				if r := recover(); r != nil {
					ret.Err = newPanicError(r)
				}
			}
		}()

		ret.Val, ret.ExpiresAt, ret.Err = fn(c.ctx, key)
		normalReturn = true
	}()

	if !normalReturn {
		recovered = true
	}
}

// Forget tells the singleflight to forget about a key.  Future calls
// to Do for this key will call the function rather than waiting for
// an earlier call to complete.
func (g *Group[K, V]) Forget(key K) {
	g.m.Delete(key)
}

// GC deletes the expired items from the cache.
func (g *Group[K, V]) GC() {
	now := nowFunc()
	g.m.Range(func(key, value any) bool {
		e := value.(*entry[V])
		e.mu.RLock()
		if !now.Before(e.expiresAt) && (e.call == nil || e.call.runs == 0) {
			g.m.Delete(key)
		}
		e.mu.RUnlock()
		return true
	})
}
