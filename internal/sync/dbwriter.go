package sync

import "context"

// A DBWriter serializes database write operations through a single goroutine.
// DuckDB allows only one concurrent writer; this makes that constraint
// explicit and avoids contention between parallel repo workers.
//
//	repo workers ──→ DBWriter ──→ Store (single goroutine)
type DBWriter struct {
	requests chan writeRequest
	done     chan struct{}
}

type writeRequest struct {
	fn   func() error
	errc chan<- error
}

// NewDBWriter starts a writer goroutine. bufferSize controls how many
// write operations can be queued before callers block on submission.
func NewDBWriter(bufferSize int) *DBWriter {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	w := &DBWriter{
		requests: make(chan writeRequest, bufferSize),
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *DBWriter) run() {
	defer close(w.done)
	for req := range w.requests {
		req.errc <- req.fn()
	}
}

// Write submits fn to the writer goroutine and blocks until it completes.
// Returns fn's error, or ctx.Err() if the context is cancelled before
// the write completes.
func (w *DBWriter) Write(ctx context.Context, fn func() error) error {
	errc := make(chan error, 1)
	select {
	case w.requests <- writeRequest{fn: fn, errc: errc}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close drains pending writes and stops the writer goroutine.
// All callers must have finished submitting writes before calling Close.
func (w *DBWriter) Close() {
	close(w.requests)
	<-w.done
}
