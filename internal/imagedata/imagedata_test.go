package imagedata

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vndroid/istore/internal/imagetype"
)

func TestReferenceLifetime(t *testing.T) {
	d := NewFromBytesWithFormat(imagetype.PNG, []byte("payload"))
	var canceled atomic.Int32
	d.AddCancel(func() { canceled.Add(1) })
	r := d.Ref()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if canceled.Load() != 0 {
		t.Fatal("cleanup ran while a reference remained")
	}
	if size, _ := r.Size(); size != len("payload") {
		t.Fatalf("size = %d", size)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if canceled.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", canceled.Load())
	}
}

func TestConcurrentReferences(t *testing.T) {
	const n = 100
	d := NewFromBytesWithFormat(imagetype.JPEG, []byte("x"))
	var canceled atomic.Int32
	d.AddCancel(func() { canceled.Add(1) })
	var wg sync.WaitGroup
	refs := make(chan ImageData, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs <- d.Ref()
		}()
	}
	wg.Wait()
	close(refs)
	for r := range refs {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if canceled.Load() != 0 {
		t.Fatal("cleanup ran before the original reference closed")
	}
	d.Close()
	if canceled.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", canceled.Load())
	}
}

func TestRefAfterLastClosePanics(t *testing.T) {
	d := NewFromBytesWithFormat(imagetype.PNG, nil)
	d.Close()
	defer func() {
		if recover() == nil {
			t.Fatal("Ref after final Close did not panic")
		}
	}()
	d.Ref()
}
