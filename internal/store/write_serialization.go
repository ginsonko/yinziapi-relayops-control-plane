package store

import (
	"context"
	"time"
)

const serializedWriteAttempts = 4

// acquireWriteGate queues only the known heavy SQLite write transactions.
// Waiting callers do not hold a database connection or an open transaction.
func (s *Store) acquireWriteGate(ctx context.Context) (func(), error) {
	select {
	case s.writeGate <- struct{}{}:
		return func() { <-s.writeGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func serializedWriteResult[T any](ctx context.Context, s *Store, operation func() (T, error)) (T, error) {
	var zero T
	release, err := s.acquireWriteGate(ctx)
	if err != nil {
		return zero, err
	}
	defer release()

	for attempt := 0; attempt < serializedWriteAttempts; attempt++ {
		result, err := operation()
		if err == nil || !isSQLiteBusy(err) || attempt == serializedWriteAttempts-1 {
			return result, err
		}
		timer := time.NewTimer(time.Duration(25*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, nil
}

func serializedWrite(ctx context.Context, s *Store, operation func() error) error {
	_, err := serializedWriteResult(ctx, s, func() (struct{}, error) {
		return struct{}{}, operation()
	})
	return err
}
