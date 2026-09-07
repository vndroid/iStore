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

import "sync"

type call struct {
	wg  sync.WaitGroup
	val any
	err error
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
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	// A panic in fn must not leave every waiter blocked forever, so the map
	// entry is removed and the WaitGroup released on the way out either way.
	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		c.wg.Done()
	}()

	c.val, c.err = fn()
	return c.val, c.err, false
}
