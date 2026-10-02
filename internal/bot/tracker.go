package bot

import (
	"context"
	"sync"
)

// Tracker counts in-flight message handlers so shutdown can wait for them.
// Unlike a bare sync.WaitGroup it stops admitting work once Close is called,
// so a handler can never call Add while Close is already waiting. The zero
// value is ready to use.
type Tracker struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// Do runs f synchronously and reports true, or reports false without
// running f once Close has been called.
//
// Parameters:
//   - f (func()): the work to track.
func (t *Tracker) Do(f func()) bool {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	t.wg.Add(1)
	t.mu.Unlock()

	defer t.wg.Done()
	f()
	return true
}

// Close stops admitting work and waits for every running Do call to return,
// or for ctx to end, whichever comes first.
//
// Parameters:
//   - ctx (context.Context): bounds the wait.
func (t *Tracker) Close(ctx context.Context) error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()

	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
