package runner

import "context"

// Close atomically rejects new runs, cancels every active call, and waits until they
// clean up or ctx expires. It is safe to call repeatedly and concurrently.
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	for reg := range r.active {
		reg.cancel()
	}
	r.mu.Unlock()

	r.closeOnce.Do(func() {
		go func() {
			r.wg.Wait()
			close(r.closeDone)
		}()
	})

	select {
	case <-r.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
