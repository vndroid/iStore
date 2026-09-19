// Package singleflight collapses concurrent calls for the same key into one.
//
// This is the same idea as golang.org/x/sync/singleflight, reimplemented here
// because iStore's whole point is a near-empty dependency graph and this is
// forty lines.
//
// It matters more than it looks: without it, a cold cache plus a page that
// references the same image twelve times means twelve simultaneous AVIF encodes
// of the same file, each ~150 ms of CPU, all producing identical bytes.
package singleflight

import (
	"fmt"
	"sync"
)

// PanicError is a recovered execution panic shared with all callers of its
// flight. Callers should treat it as an internal failure, never bad input.
type PanicError struct{ Value any }

func (e PanicError) Error() string { return fmt.Sprintf("singleflight: work panicked: %v", e.Value) }

type call struct {
	wg   sync.WaitGroup
	val  any
	err  error
	dups int
}

// Group collapses duplicate calls.
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do runs fn for key, or waits for an in-flight call with the same key and
// returns its result. shared reports whether the result came from another
// caller's execution.
func (g *Group) Do(key string, fn func() (any, error)) (v any, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	// A panic must become one shared error. Merely releasing the waiters with a
	// nil value would make every caller panic again on its result assertion.
	defer func() {
		if p := recover(); p != nil {
			c.val = nil
			c.err = PanicError{Value: p}
			v, err = nil, c.err
		}
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		c.wg.Done()
	}()

	c.val, c.err = fn()
	return c.val, c.err, false
}
