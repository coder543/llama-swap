package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/require"
)

type notifyingBody struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *notifyingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

func TestServer_RequestOrderSlowBody(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprintf("global_limit_%d", limit), func(t *testing.T) {
			entered := make(chan string, 2)
			releaseFirst := make(chan struct{})
			local := newStubRouter([]string{"first", "second"}, "")
			local.admitHTTP = func(r *http.Request) {
				data, err := swaputil.FetchContext(r, config.Config{})
				if err != nil {
					t.Error(err)
					return
				}
				entered <- data.ModelID
			}
			local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
				data, err := swaputil.FetchContext(r, config.Config{})
				if err != nil {
					t.Error(err)
					return
				}
				if data.ModelID == "first" {
					<-releaseFirst
				}
				w.WriteHeader(http.StatusOK)
			}
			s := newTestServerWithConfig(config.Config{GlobalConcurrencyLimit: limit}, local, newStubRouter(nil, ""))
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			first := chatRequest("first")
			firstBody := &notifyingBody{ReadCloser: reader, started: make(chan struct{})}
			first.Body = firstBody
			firstDone := make(chan struct{})
			go func() { defer close(firstDone); s.ServeHTTP(httptest.NewRecorder(), first) }()
			select {
			case <-firstBody.started:
			case <-time.After(time.Second):
				t.Fatal("first body was not read")
			}
			second := chatRequest("second")
			secondBody := &notifyingBody{ReadCloser: second.Body, started: make(chan struct{})}
			second.Body = secondBody
			secondDone := make(chan struct{})
			go func() { defer close(secondDone); s.ServeHTTP(httptest.NewRecorder(), second) }()
			select {
			case <-secondBody.started:
			case <-time.After(time.Second):
				t.Fatal("second body was not read")
			}
			select {
			case got := <-entered:
				t.Fatalf("%s bypassed slow first request", got)
			case <-time.After(50 * time.Millisecond):
			}
			go func() { _, _ = io.WriteString(writer, `{"model":"first"}`); _ = writer.Close() }()
			select {
			case got := <-entered:
				require.Equal(t, "first", got)
			case <-time.After(time.Second):
				t.Fatal("first request did not dispatch")
			}
			if limit == 1 {
				select {
				case got := <-entered:
					t.Fatalf("%s bypassed global limit", got)
				case <-time.After(50 * time.Millisecond):
				}
			} else {
				select {
				case got := <-entered:
					require.Equal(t, "second", got, "requests should run in parallel after admission")
				case <-time.After(time.Second):
					t.Fatal("second request did not run while the first was serving")
				}
			}
			close(releaseFirst)
			if limit == 1 {
				select {
				case got := <-entered:
					require.Equal(t, "second", got)
				case <-time.After(time.Second):
					t.Fatal("second request did not dispatch")
				}
			}
			select {
			case <-firstDone:
			case <-time.After(time.Second):
				t.Fatal("first did not finish")
			}
			select {
			case <-secondDone:
			case <-time.After(time.Second):
				t.Fatal("second did not finish")
			}
		})
	}
}

func TestServer_RequestOrderCancellation(t *testing.T) {
	order := &requestOrder{}
	first, middle, last := order.register(), order.register(), order.register()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, middle.Wait(ctx))
	middle.Release()
	middle.Release()
	require.True(t, first.Wait(context.Background()))
	first.Release()
	first.Release()
	require.True(t, last.Wait(context.Background()))
	last.Release()
	require.Zero(t, order.queue.Len())
	first, last = order.register(), order.register()
	require.False(t, first.Wait(ctx))
	first.Release()
	require.True(t, last.Wait(context.Background()))
	last.Release()
}

func TestServer_RequestOrderEarlyReturn(t *testing.T) {
	orderMW := createRequestOrderMiddleware(&requestOrder{})
	entry := make(chan struct{})
	release := make(chan struct{})
	h := orderMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "first") {
			close(entry)
			<-release
			return
		}
		if !swaputil.WaitRequestTurn(r.Context()) {
			t.Error("turn cancelled")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/first", nil))
	}()
	<-entry
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/second", nil))
	}()
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("early return did not release turn")
	}
}

func TestServer_RequestOrderAcrossUpstreamAndInference(t *testing.T) {
	entered := make(chan string, 2)
	local := newStubRouter([]string{"first", "second"}, "")
	local.admitHTTP = func(r *http.Request) {
		data, err := swaputil.FetchContext(r, config.Config{})
		if err != nil {
			t.Error(err)
			return
		}
		entered <- data.ModelID
	}
	s := newTestServerWithConfig(config.Config{Models: map[string]config.ModelConfig{"first": {}, "second": {}}}, local, newStubRouter(nil, ""))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	first := chatRequest("first")
	firstBody := &notifyingBody{ReadCloser: reader, started: make(chan struct{})}
	first.Body = firstBody
	done := make(chan struct{}, 2)
	go func() { s.ServeHTTP(httptest.NewRecorder(), first); done <- struct{}{} }()
	select {
	case <-firstBody.started:
	case <-time.After(time.Second):
		t.Fatal("first body was not read")
	}
	go func() {
		s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/upstream/second/v1/models", nil))
		done <- struct{}{}
	}()
	select {
	case got := <-entered:
		t.Fatalf("%s bypassed the earlier request", got)
	case <-time.After(50 * time.Millisecond):
	}
	go func() { _, _ = io.WriteString(writer, `{"model":"first"}`); _ = writer.Close() }()
	for _, want := range []string{"first", "second"} {
		select {
		case got := <-entered:
			require.Equal(t, want, got)
		case <-time.After(time.Second):
			t.Fatalf("%s did not dispatch", want)
		}
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("request did not finish")
		}
	}
}
