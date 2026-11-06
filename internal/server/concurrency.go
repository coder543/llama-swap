package server

import (
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"golang.org/x/sync/semaphore"
)

// CreateConcurrencyLimitMiddleware returns middleware that caps the number of
// inference requests served at once across all models, using a weighted
// semaphore. Requests wait for a slot, or stop waiting if their client cancels.
// Callers must not add it when limit <= 0; there is no "no limit" mode here.
func CreateConcurrencyLimitMiddleware(limit int) chain.Middleware {
	sem := semaphore.NewWeighted(int64(limit))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := sem.Acquire(r.Context(), 1); err != nil {
				return
			}
			defer sem.Release(1)
			next.ServeHTTP(w, r)
		})
	}
}
