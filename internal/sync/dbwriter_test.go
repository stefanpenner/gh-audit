package sync

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDBWriterBasic(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	called := false
	err := w.Write(context.Background(), func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, called, "write function was not called")
}

func TestDBWriterPropagatesError(t *testing.T) {
	w := NewDBWriter(8)
	defer w.Close()

	want := errors.New("db failure")
	got := w.Write(context.Background(), func() error {
		return want
	})
	require.ErrorIs(t, got, want)
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
	require.Len(t, order, 40)
	for i := 0; i < len(order); i += 2 {
		require.Equal(t, 1, order[i], "writes were not serialized at index %d", i)
		require.Equal(t, 2, order[i+1], "writes were not serialized at index %d", i+1)
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
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	require.Equal(t, int64(100), count.Load())
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
	require.ErrorIs(t, err, context.Canceled)

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

	require.Equal(t, int64(50), count.Load())
}

func TestDBWriterDefaultBufferSize(t *testing.T) {
	w := NewDBWriter(0) // Should default to 64
	defer w.Close()

	err := w.Write(context.Background(), func() error { return nil })
	require.NoError(t, err)
}
