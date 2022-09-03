package memoize_test

import (
	"context"
	"fmt"
	"time"

	"github.com/shogo82148/memoize"
)

func ExampleGroup_Do() {
	var g memoize.Group[string, string]
	fn := func(ctx context.Context, key string) (v string, expiredAt time.Time, err error) {
		fmt.Println("fn is just called once!")

		// it takes 100 ms to execute this function.
		time.Sleep(100 * time.Millisecond)

		// the cache is available in 1 second.
		expiredAt = time.Now().Add(time.Second)
		return
	}

	// duplicate function calls of Do just call fn once.
	go g.Do(context.Background(), "key", fn)
	go g.Do(context.Background(), "key", fn)
	g.Do(context.Background(), "key", fn)

	// this call returns the cached result.
	time.Sleep(900 * time.Millisecond)
	g.Do(context.Background(), "key", fn)

	// Output:
	// fn is just called once!
}
