package swaputil

import (
	"context"
	"sync"
)

type modelResponseLeaseKey struct{}

// ModelResponseLease holds the local model's process and scheduler slots while
// the metrics worker contacts its tokenizer. The request goroutine registers
// all releases before handing the lease to the worker.
type ModelResponseLease struct {
	releases []func()
	once     sync.Once
}

func WithModelResponseLease(ctx context.Context) (context.Context, *ModelResponseLease) {
	lease := &ModelResponseLease{}
	return context.WithValue(ctx, modelResponseLeaseKey{}, lease), lease
}

// ReleaseModelAfterResponse transfers an in-flight slot to the response lease
// when present. Otherwise the slot is released immediately.
func ReleaseModelAfterResponse(ctx context.Context, release func()) {
	if lease, ok := ctx.Value(modelResponseLeaseKey{}).(*ModelResponseLease); ok {
		lease.releases = append(lease.releases, release)
		return
	}
	release()
}

// Release returns the model's slots. Either the worker or its timeout can do
// this, so repeated or concurrent calls have no effect.
func (lease *ModelResponseLease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		for _, release := range lease.releases {
			release()
		}
		lease.releases = nil
	})
}
