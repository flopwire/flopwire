package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A shutdown waits for a request in flight, here one that answers once the
// server is stopping (as a bus poll does), before serve returns (#70).
func TestServeUntilDoneWaitsForRequestsInFlight(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	entered := make(chan struct{})
	var finished atomic.Bool
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-ctx.Done()
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		_, _ = io.WriteString(w, "answered")
	})}
	served := make(chan error, 1)
	go func() {
		served <- serveUntilDone(ctx, server, 10*time.Second, func() error { return server.Serve(ln) })
	}()
	type reply struct {
		body string
		err  error
	}
	got := make(chan reply, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- reply{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- reply{string(b), err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("request never reached the handler")
	}
	stop()
	select {
	case err := <-served:
		if err != nil || !finished.Load() {
			t.Fatalf("serve returned %v before the request in flight finished (%v)", err, finished.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
	if r := <-got; r.err != nil || r.body != "answered" {
		t.Fatalf("request in flight: %q %v", r.body, r.err)
	}
}
