package client

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/scottjab/folio/internal/events"
)

// maxEventBytes caps one SSE frame. Frames carry a path and a hash, never a
// note's contents, so anything near this is a bug rather than a big note.
const maxEventBytes = 1 << 20

// reconnectMin and reconnectMax bound the backoff between reconnect attempts.
const (
	reconnectMin = 500 * time.Millisecond
	reconnectMax = 30 * time.Second
)

// Event is one message from the server's stream: either a note changed, or the
// connection itself did.
type Event struct {
	// Change is set when a note was created, updated, deleted, or moved.
	Change *events.NoteChanged
	// Connected reports the state of the stream. It is meaningful only when
	// Change is nil, which is how a connect or a drop is announced.
	Connected bool
	// Err says why the stream dropped. The stream reconnects on its own, so this
	// is information for the UI rather than something the caller must handle.
	Err error
}

// Watch subscribes to note changes and returns a channel of events.
//
// The channel is closed when ctx is cancelled, and only then: a dropped
// connection is reported as an event and retried with backoff, because a server
// restart or a laptop lid closing should not end a running UI. Nothing is
// buffered beyond a small queue, so a caller that stops reading blocks the
// stream rather than growing memory without bound.
func (c *Client) Watch(ctx context.Context) <-chan Event {
	out := make(chan Event, 16)
	go func() {
		defer close(out)

		// No overall timeout: an SSE response is meant to stay open, and the
		// client's normal 30s deadline would kill it on the dot.
		hc := *c.http
		hc.Timeout = 0

		var lastID string
		backoff := reconnectMin
		for ctx.Err() == nil {
			id, err := c.stream(ctx, &hc, lastID, out)
			if id != "" {
				lastID = id
			}
			if ctx.Err() != nil {
				return
			}
			send(ctx, out, Event{Connected: false, Err: err})

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > reconnectMax {
				backoff = reconnectMax
			}
			// A connection that lived long enough to deliver anything means the
			// server is healthy, so the next drop should retry promptly. That is
			// handled by resetting inside stream, below.
			if id != "" {
				backoff = reconnectMin
			}
		}
	}()
	return out
}

// stream holds one connection open, returning the last event id it saw and why
// it ended.
func (c *Client) stream(ctx context.Context, hc *http.Client, lastID string, out chan<- Event) (string, error) {
	u := *c.base
	u.Path = c.base.Path + "/api/events"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", c.ua)
	// Not used by the server yet, but it is the standard way to say where we
	// left off and costs nothing to send.
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", apiError(request{Method: http.MethodGet, Path: "/api/events"}, resp)
	}
	send(ctx, out, Event{Connected: true})

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 8<<10), maxEventBytes)

	var (
		kind    string
		data    strings.Builder
		seen    string
		anyData bool
	)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")

		if line == "" { // end of frame
			if anyData {
				var e events.NoteChanged
				if err := json.Unmarshal([]byte(data.String()), &e); err == nil {
					if e.Kind == "" {
						e.Kind = kind
					}
					if e.ID != "" {
						seen = e.ID
					}
					send(ctx, out, Event{Change: &e, Connected: true})
				}
				// A frame we cannot parse is skipped rather than killing the
				// stream: the connection is still good, and the next event is
				// probably fine.
			}
			kind, anyData = "", false
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") { // keepalive comment
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			seen = value
		case "event":
			kind = value
		case "data":
			if anyData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			anyData = true
		}
	}
	err = sc.Err()
	if err == nil {
		err = io.EOF
	}
	if errors.Is(err, context.Canceled) {
		return seen, nil
	}
	return seen, err
}

// send delivers an event unless the caller has gone away.
func send(ctx context.Context, out chan<- Event, e Event) {
	select {
	case out <- e:
	case <-ctx.Done():
	}
}
