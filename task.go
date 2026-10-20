package main

import (
	"context"
	"sync"
)

// taskContext pauses cooperatively. Cancellation also wakes paused workers,
// so handles are released before the temporary archive is removed.
type taskContext struct {
	context.Context
	mu     sync.Mutex
	resume chan struct{}
}

type pauseKey struct{}

func (t *taskContext) Value(key any) any {
	if _, ok := key.(pauseKey); ok {
		return t
	}
	return t.Context.Value(key)
}

func (t *taskContext) pause() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resume != nil {
		close(t.resume)
		t.resume = nil
		return false
	}
	t.resume = make(chan struct{})
	return true
}

func (t *taskContext) wait() error {
	return t.waitWith(t.Context)
}
func (t *taskContext) waitWith(ctx context.Context) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		t.mu.Lock()
		ch := t.resume
		t.mu.Unlock()
		if ch == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

func checkpoint(ctx context.Context) error {
	if t, ok := ctx.Value(pauseKey{}).(*taskContext); ok {
		return t.waitWith(ctx)
	}
	return ctx.Err()
}
