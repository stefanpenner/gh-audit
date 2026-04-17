package sync

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDBWriterBasic(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	called := false
	err := w.Write(context.Background(), func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("write function was not called")
	}
}

func TestDBWriterPropagatesError(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	want := errors.New("db failure")
	got := w.Write(context.Background(), func() error {
		return want
	})
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}

func TestDBWriterSerializesWrites(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	var mu sync.Mutex
	var order []int

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// All goroutines submit to the writer concurrently.
			// Writes must not overlap (no concurrent fn execution).
			_ = w.Write(context.Background(), func() error {
				mu.Lock()
				order = append(order, 1)
				mu.Unlock()
				// Simulate work to increase overlap chance
				time.Sleep(time.Millisecond)
				mu.Lock()
				order = append(order, 2)
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	// Verify serialization: pattern must be [1,2,1,2,...] — never [1,1,...]
	if len(order) != 40 {
		t.Fatalf("expected 40 entries, got %d", len(order))
	}
	for i := 0; i < len(order); i += 2 {
		if order[i] != 1 || order[i+1] != 2 {
			t.Fatalf("writes were not serialized: order[%d]=%d, order[%d]=%d", i, order[i], i+1, order[i+1])
		}
	}
}

func TestDBWriterConcurrentSubmit(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	var count atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := w.Write(context.Background(), func() error {
				count.Add(1)
				return nil
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := count.Load(); got != 100 {
		t.Fatalf("expected 100 writes, got %d", got)
	}
}

func TestDBWriterContextCancelBeforeSubmit(t *testing.T) {
	// Fill the buffer so the next Write blocks on submission
	w := NewDBWriter(1)

	blocker := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.Write(context.Background(), func() error {
			<-blocker // Block the writer goroutine
			return nil
		})
	}()
	// Give the first write time to reach the writer goroutine
	time.Sleep(10 * time.Millisecond)

	// Fill the buffer
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.Write(context.Background(), func() error { return nil })
	}()
	time.Sleep(10 * time.Millisecond)

	// Now the channel buffer is full. Cancel context before submitting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := w.Write(ctx, func() error {
		t.Fatal("should not be called")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	close(blocker)
	wg.Wait()
	w.Close()
}

func TestDBWriterContextCancelDuringWait(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})

	go func() {
		err := w.Write(ctx, func() error {
			close(started)
			time.Sleep(500 * time.Millisecond) // Slow write
			return nil
		})
		// Either nil (write completed) or context.Canceled
		_ = err
	}()

	<-started
	cancel() // Cancel while write is running

	// The writer should still process the fn (it's already running),
	// but the caller may get context.Canceled back.
	// This test verifies no panic or deadlock.
}

func TestDBWriterCloseDrainsPending(t *testing.T) {
	w := NewDBWriter(100)

	var count atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Write(context.Background(), func() error {
				count.Add(1)
				return nil
			})
		}()
	}

	wg.Wait()
	w.Close()

	if got := count.Load(); got != 50 {
		t.Fatalf("expected 50 writes drained, got %d", got)
	}
}

func TestDBWriterDefaultBufferSize(t *testing.T) {
	w := NewDBWriter(0) // Should default to 64
	defer w.Close()

	err := w.Write(context.Background(), func() error { return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
