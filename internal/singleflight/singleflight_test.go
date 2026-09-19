package singleflight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForDuplicates(t *testing.T, g *Group, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		got := 0
		if c := g.m[key]; c != nil {
			got = c.dups
		}
		g.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("did not observe %d duplicate callers", want)
}

func TestDoCoalescesAndReportsShared(t *testing.T) {
	const n = 12
	var g Group
	var calls atomic.Int32
	release := make(chan struct{})
	type result struct {
		value  any
		err    error
		shared bool
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err, shared := g.Do("same", func() (any, error) {
				calls.Add(1)
				<-release
				return "value", nil
			})
			results <- result{v, err, shared}
		}()
	}
	waitForDuplicates(t, &g, "same", n-1)
	close(release)
	wg.Wait()
	close(results)

	shared := 0
	for r := range results {
		if r.err != nil || r.value != "value" {
			t.Fatalf("result = (%v, %v)", r.value, r.err)
		}
		if r.shared {
			shared++
		}
	}
	if calls.Load() != 1 || shared != n-1 {
		t.Fatalf("calls = %d, shared = %d; want 1, %d", calls.Load(), shared, n-1)
	}
}

func TestDoPanicReturnsOneSharedError(t *testing.T) {
	const n = 12
	var g Group
	release := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err, _ := g.Do("panic", func() (any, error) {
				<-release
				panic("boom")
			})
			errs <- err
		}()
	}
	waitForDuplicates(t, &g, "panic", n-1)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		var panicErr PanicError
		if !errors.As(err, &panicErr) || panicErr.Value != "boom" {
			t.Fatalf("error = %#v, want PanicError(boom)", err)
		}
	}
	if v, err, shared := g.Do("panic", func() (any, error) { return 7, nil }); err != nil || v != 7 || shared {
		t.Fatalf("retry = (%v, %v, %v), want (7, nil, false)", v, err, shared)
	}
}
