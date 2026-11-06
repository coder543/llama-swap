package server

import (
	"container/list"
	"context"
	"net/http"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// requestOrder assigns a turn before body parsing, while allowing parsing to
// proceed concurrently. Only the oldest request may enter model dispatch.
type requestOrder struct {
	mu    sync.Mutex
	queue list.List
}

type requestTurn struct {
	order   *requestOrder
	element *list.Element
	ready   chan struct{}
}

func (o *requestOrder) register() *requestTurn {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := &requestTurn{order: o, ready: make(chan struct{})}
	t.element = o.queue.PushBack(t)
	if o.queue.Len() == 1 {
		close(t.ready)
	}
	return t
}

func (t *requestTurn) Wait(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-t.ready:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (t *requestTurn) Release() {
	o := t.order
	o.mu.Lock()
	defer o.mu.Unlock()
	if t.element == nil {
		return
	}
	wasHead := o.queue.Front() == t.element
	o.queue.Remove(t.element)
	t.element = nil
	if wasHead && o.queue.Front() != nil {
		close(o.queue.Front().Value.(*requestTurn).ready)
	}
}

func createRequestOrderMiddleware(order *requestOrder) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if swaputil.HasRequestTurn(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			turn := order.register()
			defer turn.Release()
			next.ServeHTTP(w, r.WithContext(swaputil.WithRequestTurn(r.Context(), turn)))
		})
	}
}

func waitRequestTurn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if swaputil.WaitRequestTurn(r.Context()) {
			next.ServeHTTP(w, r)
		}
	})
}
