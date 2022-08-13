package memoize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Group memoizes the calls of Func with expiration.
type Group[K comparable, V any] struct {
	mu sync.Mutex      // protects m
	m  map[K]*entry[V] // lazy initialized
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
	chans  []chan<- result[V]
}

type result[V any] struct {
	val       V
	expiresAt time.Time
	err       error
}

// Do calls memoized Func.
func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(ctx context.Context, key K) (val V, expiresAt time.Time, err error)) (V, time.Time, error) {
	now := nowFunc()

	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[K]*entry[V])
	}
	var e *entry[V]
	var ok bool
	if e, ok = g.m[key]; ok {
		e.mu.RLock()
		val, expiresAt := e.val, e.expiresAt
		e.mu.RUnlock()
		if now.Before(expiresAt) {
			// the cache is available.
			g.mu.Unlock()
			return val, expiresAt, nil
		}
	} else {
		// there is no entry. create a new one.
		e = &entry[V]{}
		g.m[key] = e
	}
	g.mu.Unlock()

	// the cache is expired or unavailable.
	e.mu.Lock()
	c := e.call
	if c == nil {
		// it is the first call.
		c = new(call[V])
		c.ctx, c.cancel = context.WithCancel(&detachedContext{ctx})
		e.call = c
		go do(g, e, c, key, fn)
	}
	ch := make(chan result[V], 1)
	c.chans = append(c.chans, ch)
	c.runs++
	e.mu.Unlock()

	select {
	case ret := <-ch:
		return ret.val, ret.expiresAt, ret.err
	case <-ctx.Done():
		e.mu.Lock()
		c.runs--
		if c.runs == 0 {
			c.cancel()
			e.call = nil // to avoid adding new channels to c.chans
		}
		e.mu.Unlock()
		var zero V
		return zero, time.Time{}, ctx.Err()
	}
}

func do[K comparable, V any](g *Group[K, V], e *entry[V], c *call[V], key K, fn func(ctx context.Context, key K) (V, time.Time, error)) {
	var ret result[V]

	normalReturn := false
	recovered := false

	// use double-defer to distinguish panic from runtime.Goexit,
	// more details see https://golang.org/cl/134395
	defer func() {
		// the given function invoked runtime.Goexit
		if !normalReturn && !recovered {
			ret.err = errGoexit
		}

		// save to the cache
		e.mu.Lock()
		e.call = nil // to avoid adding new channels to c.chans
		chans := c.chans
		if ret.err == nil {
			e.val = ret.val
			e.expiresAt = ret.expiresAt
		}
		e.mu.Unlock()

		if e, ok := ret.err.(*panicError); ok {
			panic(e)
		} else if ret.err == errGoexit {
			// Already in the process of goexit, no need to call again
		} else {
			// Normal return
			// notify the result to the callers
			for _, ch := range chans {
				ch <- ret
			}
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
					ret.err = newPanicError(r)
				}
			}
		}()

		ret.val, ret.expiresAt, ret.err = fn(c.ctx, key)
		normalReturn = true
	}()

	if !normalReturn {
		recovered = true
	}
}

func (g *Group[K, V]) GC() {
	now := nowFunc()
	g.mu.Lock()
	for key, e := range g.m {
		e.mu.RLock()
		if !now.Before(e.expiresAt) && (e.call == nil || e.call.runs == 0) {
			delete(g.m, key)
		}
		e.mu.RUnlock()
	}
	g.mu.Unlock()
}
