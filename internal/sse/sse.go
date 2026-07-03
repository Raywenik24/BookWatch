// Package sse is a small, generic server-sent-events broker: one place to
// fan a stream of named events out to every connected client. It knows nothing
// about what it carries — callers Publish(name, payload) and the broker
// JSON-encodes the payload and writes a well-formed SSE frame to each
// subscriber. Written to be lifted into other projects wholesale (BookWatch's
// only use today is the live check-progress bar, issue #47).
//
// Auth is intentionally out of scope here: wrap ServeHTTP in whatever
// middleware the host app uses. BookWatch guards it with the same
// X-BookWatch-Token header check as every other write route, which means the
// browser's native EventSource (no custom headers) can't consume it — the
// bundled client uses fetch + ReadableStream instead (see web/index.html).
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// keepAlive is how often an idle stream emits a comment line. It keeps
// intermediary proxies from reaping a quiet connection and lets the server
// notice a client that vanished without a clean close (the write fails).
const keepAlive = 25 * time.Second

// subBuffer is the per-subscriber channel depth. A slow client that can't keep
// up past this many buffered events gets its oldest events dropped rather than
// stalling every other subscriber (Publish never blocks — see below).
const subBuffer = 16

// Broker fans published events out to all current subscribers. The zero value
// is not usable; call New.
type Broker struct {
	mu   sync.Mutex
	subs map[chan message]struct{}
}

// message is one already-encoded event queued for a subscriber.
type message struct {
	name string
	data []byte
}

// New returns a ready Broker.
func New() *Broker {
	return &Broker{subs: make(map[chan message]struct{})}
}

// Publish JSON-encodes payload and delivers it to every subscriber as an event
// named `event`. It never blocks: a subscriber whose buffer is full has its
// oldest queued event dropped to make room, so one stalled client can't wedge
// the publisher (which, for BookWatch, is the check goroutine). A payload that
// fails to encode is dropped silently — a live progress tick isn't worth
// killing the run over.
func (b *Broker) Publish(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := message{name: event, data: data}

	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default:
			// Buffer full: drop the oldest so the newest (freshest progress)
			// still lands. The receive can race a just-drained channel, hence
			// the second select's default.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- msg:
			default:
			}
		}
	}
}

// ServeHTTP streams events to one client until it disconnects. It emits the
// standard text/event-stream headers, an initial retry hint, then each
// published event as it arrives plus a periodic keep-alive comment. Returns
// (ending the request) when the client's context is cancelled.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering (e.g. nginx) so events flush immediately.
	h.Set("X-Accel-Buffering", "no")

	ch := b.subscribe()
	defer b.unsubscribe(ch)

	// Tell the client's reconnect logic how long to wait before retrying after
	// a drop. Consumed by both native EventSource and the bundled fetch client.
	fmt.Fprintf(w, "retry: 3000\n\n")
	flusher.Flush()

	ping := time.NewTicker(keepAlive)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			// A well-formed SSE frame: event name + one data line + blank line.
			// Payloads here are single-line JSON, so no multi-line splitting is
			// needed.
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.name, msg.data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (b *Broker) subscribe() chan message {
	ch := make(chan message, subBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan message) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}
