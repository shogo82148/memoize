# memoize

[![test](https://github.com/shogo82148/memoize/actions/workflows/test.yml/badge.svg)](https://github.com/shogo82148/memoize/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/shogo82148/memoize.svg)](https://pkg.go.dev/github.com/shogo82148/memoize)

Package memoize provides a duplicate function call suppression and caching mechanism.
It is similar to [x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight), but it caches the results.

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/shogo82148/memoize"
)

func main() {
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
```
