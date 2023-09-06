//go:build !go1.21

package memoize

import (
	"context"
	"reflect"
	"time"
)

var _ context.Context = (*detachedContext)(nil)

func withoutCancel(ctx context.Context) context.Context {
	return &detachedContext{ctx}
}

// detachedContext is never canceled and no deadline.
// whether it has values.
type detachedContext struct {
	context.Context
}

func (*detachedContext) Deadline() (deadline time.Time, ok bool) {
	return
}

func (*detachedContext) Done() <-chan struct{} {
	return nil
}

func (*detachedContext) Err() error {
	return nil
}

type stringer interface {
	String() string
}

func contextName(c context.Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflect.TypeOf(c).String()
}

func (ctx *detachedContext) String() string {
	return contextName(ctx.Context) + ".Detached"
}
