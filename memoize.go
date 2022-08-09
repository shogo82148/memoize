package memoize

import (
	"context"
	"sync"
	"time"
)

// for testing
var nowFunc = time.Now

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
func (g *Group[K, V]) Do(ctx context.Context, key K, fn func(ctx context.Context) (val V, expiresAt time.Time, err error)) (V, time.Time, error) {
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
		c.ctx, c.cancel = context.WithCancel(context.Background())
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

func do[K comparable, V any](g *Group[K, V], e *entry[V], c *call[V], key K, fn func(ctx context.Context) (V, time.Time, error)) {
	defer c.cancel()

	v, expiresAt, err := fn(c.ctx)
	ret := result[V]{
		val:       v,
		expiresAt: expiresAt,
		err:       err,
	}

	// save to the cache
	e.mu.Lock()
	e.call = nil // to avoid adding new channels to c.chans
	chans := c.chans
	if err == nil {
		e.val = v
		e.expiresAt = expiresAt
	}
	e.mu.Unlock()

	// notify the result to the callers
	for _, ch := range chans {
		ch <- ret
	}
}
