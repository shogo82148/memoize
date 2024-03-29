package memoize

import (
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDo(t *testing.T) {
	ttl := time.Second
	now := time.Unix(1234567890, 0)
	nowFunc = func() time.Time {
		return now
	}
	defer func() {
		nowFunc = time.Now
	}()

	var calls int
	var g Group[string, int]
	fn := func(ctx context.Context, key string) (int, time.Time, error) {
		if key != "foobar" {
			t.Errorf("want foobar, got %q", key)
		}
		calls++
		return calls, now.Add(ttl), nil
	}

	// first call
	got, expiresAt, err := g.Do(context.Background(), "foobar", fn)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("want 1, got %d", got)
	}
	if expiresAt.Unix() != 1234567891 {
		t.Errorf("want %d, got %d", 1234567891, expiresAt.Unix())
	}

	// the cache is still available, fn should not be called.
	now = now.Add(ttl - 1)

	g.GC()
	got, expiresAt, err = g.Do(context.Background(), "foobar", fn)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("want 1, got %d", got)
	}
	if expiresAt.Unix() != 1234567891 {
		t.Errorf("want %d, got %d", 1234567891, expiresAt.Unix())
	}

	// the cache is expired, so fn should be called.
	now = now.Add(1)

	g.GC()
	got, expiresAt, err = g.Do(context.Background(), "foobar", fn)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("want 2, got %d", got)
	}
	if expiresAt.Unix() != 1234567892 {
		t.Errorf("want %d, got %d", 1234567892, expiresAt.Unix())
	}

	now = now.Add(ttl)
	g.GC()
}

func TestDoErr(t *testing.T) {
	var g Group[string, any]
	someErr := errors.New("Some error")
	v, expiresAt, err := g.Do(context.Background(), "key", func(ctx context.Context, _ string) (any, time.Time, error) {
		return nil, time.Time{}, someErr
	})
	if err != someErr {
		t.Errorf("Do error = %v; want someErr %v", err, someErr)
	}
	if v != nil {
		t.Errorf("unexpected non-nil value %#v", v)
	}
	if !expiresAt.IsZero() {
		t.Errorf("want expiresAt is zero, got %s", expiresAt)
	}
}

func TestDoDupSuppress(t *testing.T) {
	var g Group[string, string]
	var wg1, wg2 sync.WaitGroup
	c := make(chan string, 1)
	var calls int32
	fn := func(ctx context.Context, _ string) (string, time.Time, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// First invocation.
			wg1.Done()
		}
		v := <-c
		c <- v // pump; make available for any future calls

		time.Sleep(10 * time.Millisecond) // let more goroutines enter Do

		return v, nowFunc(), nil
	}

	const n = 10
	wg1.Add(1)
	for i := 0; i < n; i++ {
		wg1.Add(1)
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			wg1.Done()
			v, _, err := g.Do(context.Background(), "key", fn)
			if err != nil {
				t.Errorf("Do error: %v", err)
				return
			}
			if v != "bar" {
				t.Errorf("Do = %T %v; want %q", v, v, "bar")
			}
		}()
	}
	wg1.Wait()
	// At least one goroutine is in fn now and all of them have at
	// least reached the line before the Do.
	c <- "bar"
	wg2.Wait()
	if got := atomic.LoadInt32(&calls); got <= 0 || got >= n {
		t.Errorf("number of calls = %d; want over 0 and less than %d", got, n)
	}
}

func TestDoCancel(t *testing.T) {
	ch := make(chan struct{}, 1)
	ch1 := make(chan struct{}, 1)
	ch2 := make(chan struct{}, 1)
	fn := func(ctx context.Context, _ string) (any, time.Time, error) {
		<-ctx.Done() // block the execution until ctx is canceled.
		if err := ctx.Err(); err != context.Canceled {
			t.Errorf("want context.Canceled, got %v", err)
		}
		ch <- struct{}{}
		return "bar", time.Time{}, nil
	}

	var g Group[string, any]

	// goroutine 1
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		_, _, err := g.Do(ctx1, "key", fn)
		if err != context.Canceled {
			t.Errorf("want context.Canceled, got %v", err)
		}
		ch1 <- struct{}{}
	}()

	// goroutine 2
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() {
		_, _, err := g.Do(ctx2, "key", fn)
		if err != context.Canceled {
			t.Errorf("want context.Canceled, got %v", err)
		}
		ch2 <- struct{}{}
	}()

	// wait for goroutine 1 and 2 to be blocked by fn
	for {
		time.Sleep(time.Millisecond)
		if e, ok := g.m.Load("key"); ok {
			e := e.(*entry[any])
			e.mu.RLock()
			runs := e.call.runs
			e.mu.RUnlock()
			if runs == 2 {
				break
			}
		}
	}

	cancel1() // cancel goroutine 1
	select {
	case <-ch1:
		// goroutine 1 should be canceled
	case <-time.After(time.Second):
		t.Fatalf("Do hangs")
	}

	// other goroutines are still blocked
	select {
	case <-ch:
		t.Fatal("should be blocked, but ch is closed")
	case <-ch2:
		t.Fatal("should be blocked, but ch2 is closed")
	case <-time.After(10 * time.Millisecond):
	}

	cancel2() // cancel goroutine 2

	// now all goroutines are canceled
	select {
	case <-ch2:
		// goroutine 1 should be canceled
	case <-time.After(time.Second):
		t.Fatalf("Do hangs")
	}
	select {
	case <-ch:
		// goroutine 1 should be canceled
	case <-time.After(time.Second):
		t.Fatalf("Do hangs")
	}
}

func TestDoContext(t *testing.T) {
	type key struct {
		name string
	}

	var g Group[string, int]
	fn := func(ctx context.Context, _ string) (int, time.Time, error) {
		val := ctx.Value(key{"hoge"})
		if val != "fuga" {
			t.Errorf("want fuga, got %q", val)
		}
		return 0, nowFunc().Add(time.Second), nil
	}

	ctx := context.WithValue(context.Background(), key{"hoge"}, "fuga")
	_, _, err := g.Do(ctx, "foobar", fn)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPanicDo(t *testing.T) {
	var g Group[string, int]
	fn := func(ctx context.Context, _ string) (int, time.Time, error) {
		panic("something wrong!!")
	}

	const n = 5
	waited := int32(n)
	panicCount := int32(0)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() {
				if err := recover(); err != nil {
					t.Logf("Got panic: %v\n%s", err, debug.Stack())
					atomic.AddInt32(&panicCount, 1)
				}
				if atomic.AddInt32(&waited, -1) == 0 {
					close(done)
				}
			}()
			g.Do(context.Background(), "key", fn)
		}()
	}

	select {
	case <-done:
		if panicCount != n {
			t.Errorf("panic count = %d; want %d", panicCount, n)
		}
	case <-time.After(time.Second):
		t.Errorf("Do hangs")
	}
}

func TestGoexitDo(t *testing.T) {
	var g Group[string, int]
	fn := func(ctx context.Context, _ string) (int, time.Time, error) {
		runtime.Goexit()
		return 0, time.Time{}, nil
	}

	const n = 5
	waited := int32(n)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			var err error
			defer func() {
				if err != nil {
					t.Errorf("Error should be nil, but got: %v", err)
				}
				if atomic.AddInt32(&waited, -1) == 0 {
					close(done)
				}
			}()
			_, _, err = g.Do(context.Background(), "key", fn)
		}()
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("Do hangs")
	}
}

type errValue struct{}

func (err *errValue) Error() string {
	return "error value"
}

func TestPanicErrorUnwrap(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		panicValue       interface{}
		wrappedErrorType bool
	}{
		{
			name:             "panicError wraps non-error type",
			panicValue:       &panicError{value: "string value"},
			wrappedErrorType: false,
		},
		{
			name:             "panicError wraps error type",
			panicValue:       &panicError{value: new(errValue)},
			wrappedErrorType: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			var recovered interface{}
			group := new(Group[string, int])
			func() {
				defer func() {
					recovered = recover()
					t.Logf("after panic(%#v) in group.Do, recovered %#v", tc.panicValue, recovered)
				}()
				_, _, _ = group.Do(context.Background(), "key", func(ctx context.Context, _ string) (int, time.Time, error) {
					panic(tc.panicValue)
				})
			}()

			if recovered == nil {
				t.Fatal("expected a non-nil panic value")
			}

			err, ok := recovered.(error)
			if !ok {
				t.Fatalf("expected panic value to be an error, got %T", recovered)
			}

			if !errors.Is(err, new(errValue)) && tc.wrappedErrorType {
				t.Errorf("unexpected wrapped error type %T; want %T", err, new(errValue))
			}
		})
	}
}

func benchmarkDo(parallelism int) func(b *testing.B) {
	return func(b *testing.B) {
		b.SetParallelism(parallelism)
		fn := func(ctx context.Context, _ *testing.PB) (int, time.Time, error) {
			time.Sleep(5 * time.Millisecond)
			return 0, time.Now().Add(10 * time.Millisecond), nil
		}
		var g Group[*testing.PB, int]
		b.RunParallel(func(p *testing.PB) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			for p.Next() {
				g.Do(ctx, p, fn)
			}
		})
	}
}

func BenchmarkDo(b *testing.B) {
	b.Run("1", benchmarkDo(1))
	b.Run("10", benchmarkDo(10))
	b.Run("100", benchmarkDo(100))
	b.Run("1000", benchmarkDo(1000))
	b.Run("10000", benchmarkDo(10000))
}

func TestForget(t *testing.T) {
	var g Group[string, int]

	// Forget should do nothing if the key does not exist.
	g.Forget("key")

	var (
		firstStarted  = make(chan struct{})
		unblockFirst  = make(chan struct{})
		firstFinished = make(chan struct{})
	)

	go func() {
		g.Do(context.Background(), "key", func(ctx context.Context, key string) (val int, expiresAt time.Time, err error) {
			close(firstStarted)
			<-unblockFirst
			close(firstFinished)
			return
		})
	}()
	<-firstStarted
	g.Forget("key")

	unblockSecond := make(chan struct{})
	secondResult := g.DoChan(context.Background(), "key", func(ctx context.Context, key string) (val int, expiresAt time.Time, err error) {
		<-unblockSecond
		return 2, time.Time{}, nil
	})

	close(unblockFirst)
	<-firstFinished

	thirdResult := g.DoChan(context.Background(), "key", func(ctx context.Context, key string) (val int, expiresAt time.Time, err error) {
		return 3, time.Time{}, nil
	})

	close(unblockSecond)
	<-secondResult
	ret := <-thirdResult
	if ret.Val != 2 {
		t.Errorf("We should receive result produced by second call, expected: 2, got %d", ret.Val)
	}
}
