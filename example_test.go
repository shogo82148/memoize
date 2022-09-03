package memoize_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shogo82148/memoize"
)

func ExampleGroup_Do() {
	var g memoize.Group[string, string]
	var wg sync.WaitGroup
	fn := func(ctx context.Context, key string) (v string, expiredAt time.Time, err error) {
		fmt.Println("fn is called!")
		time.Sleep(time.Second)
		return "", time.Now().Add(time.Second), nil
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			g.Do(context.Background(), "key", fn)
			wg.Done()
		}()
	}
	wg.Wait()

	// Output:
	// fn is called!
}
