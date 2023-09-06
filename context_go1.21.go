//go:build go1.21

package memoize

import (
	"context"
)

func withoutCancel(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
