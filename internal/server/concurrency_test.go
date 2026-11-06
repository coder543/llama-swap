package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestServer_ConcurrencyLimitMiddleware(t *testing.T) {
	t.Run("allows requests up to the limit", func(t *testing.T) {
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mw := CreateConcurrencyLimitMiddleware(2)
		handler := mw(final)

		for i := 0; i < 2; i++ {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, http.StatusOK, w.Code)
		}
	})

	t.Run("queues once the limit is reached", func(t *testing.T) {
		// blocks until released, so the slot stays held while the next request
		// is admitted
		release := make(chan struct{})
		started := make(chan struct{})
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			w.WriteHeader(http.StatusOK)
		})
		mw := CreateConcurrencyLimitMiddleware(1)
		handler := mw(final)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
		}()

		<-started

		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		w := httptest.NewRecorder()
		secondDone := make(chan struct{})
		go func() { handler.ServeHTTP(w, r); close(secondDone) }()
		select {
		case <-secondDone:
			t.Fatal("second request returned while the slot was occupied")
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		wg.Wait()
		<-secondDone
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("released slot can be reacquired", func(t *testing.T) {
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mw := CreateConcurrencyLimitMiddleware(1)
		handler := mw(final)

		for i := 0; i < 3; i++ {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, http.StatusOK, w.Code)
		}
	})
}
