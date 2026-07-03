package sse

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readFrame reads one SSE frame (up to the blank-line separator) from r.
func readFrame(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if line == "\n" {
			return b.String()
		}
		b.WriteString(line)
	}
}

// TestServeHTTPDeliversPublishedEvent connects a subscriber, publishes, and
// confirms the JSON-encoded event arrives as a well-formed frame.
func TestServeHTTPDeliversPublishedEvent(t *testing.T) {
	b := New()
	srv := httptest.NewServer(http.HandlerFunc(b.ServeHTTP))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	r := bufio.NewReader(resp.Body)

	// First frame is the retry hint.
	if f := readFrame(t, r); !strings.Contains(f, "retry:") {
		t.Fatalf("first frame = %q, want retry hint", f)
	}

	// The subscriber must be registered before Publish, so poll until it is.
	waitFor(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.subs) == 1 })

	b.Publish("status", map[string]int{"current": 3})
	f := readFrame(t, r)
	if !strings.Contains(f, "event: status") || !strings.Contains(f, `data: {"current":3}`) {
		t.Fatalf("frame = %q, want status event with payload", f)
	}
}

// TestPublishNeverBlocks fills a subscriber's buffer past capacity and confirms
// Publish still returns promptly (oldest events dropped, newest kept), so one
// stalled client can't wedge the publisher.
func TestPublishNeverBlocks(t *testing.T) {
	b := New()
	ch := b.subscribe()
	defer b.unsubscribe(ch)

	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*4; i++ {
			b.Publish("status", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	if len(ch) > subBuffer {
		t.Fatalf("buffered %d events, want <= %d", len(ch), subBuffer)
	}
}

// TestServeHTTPExitsOnDisconnect confirms the handler returns when the client
// context is cancelled, so subscribers don't leak.
func TestServeHTTPExitsOnDisconnect(t *testing.T) {
	b := New()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { b.ServeHTTP(rec, req); close(done) }()

	waitFor(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.subs) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after client disconnect")
	}
	b.mu.Lock()
	n := len(b.subs)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("subscriber leaked: %d still registered", n)
	}
}

// waitFor polls cond until true or a 2s deadline.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
