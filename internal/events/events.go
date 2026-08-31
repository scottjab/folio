// Package events is a tiny in-process pub/sub bus.
//
// It exists so the pieces that need to know a note changed (the SSE stream
// feeding the browser, the MCP server pushing resource-updated notifications,
// the indexer) can find out without the writer having to know they exist.
//
// The API is built on Go 1.27 generic methods. Before 1.27 a method could not
// declare type parameters, so a typed bus had to expose package-level functions
// with the bus passed in:
//
//	events.On[NoteChanged](bus, handler)
//	events.Emit(bus, NoteChanged{...})
//
// which reads backwards and puts the bus in the argument list of everything.
// Now the type parameter lives on the method, the receiver is the bus again, and
// the event type is inferred from the handler:
//
//	bus.On(func(ctx context.Context, e NoteChanged) { ... })
//	bus.Emit(ctx, NoteChanged{...})
package events

import (
	"context"
	"log/slog"
	"reflect"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// queueDepth is how many undelivered events one subscriber may fall behind by.
// Events are small and subscribers are fast; this is a backstop against a wedged
// consumer, not a normal operating condition.
const queueDepth = 256

// Bus routes events to handlers by event type.
//
// Each subscriber gets its own serial queue, which buys two properties that
// matter:
//
//   - Emit never blocks, so a stalled SSE client cannot hold up the HTTP handler
//     that just saved a note.
//   - A subscriber sees events in the order they were emitted. Without this, a
//     "note deleted" could overtake the "note created" that preceded it and the
//     browser would end up showing a note that is gone.
//
// Under sustained backpressure a subscriber's queue fills and further events for
// that subscriber are dropped and counted, rather than growing without bound.
type Bus struct {
	mu     sync.RWMutex
	subs   map[reflect.Type]map[uint64]*subscription
	nextID uint64

	wg      sync.WaitGroup
	dropped atomic.Int64

	// Log receives handler panics and drop warnings. Nil means slog.Default.
	Log *slog.Logger
}

// subscription is one registered handler and its private delivery queue.
type subscription struct {
	fn   any // func(context.Context, E) for this subscription's E
	jobs chan func()
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[reflect.Type]map[uint64]*subscription{}}
}

// On registers a handler for events of type E and returns a function that
// unsubscribes it. The returned function is safe to call more than once.
//
// E is inferred from fn, so callers write the handler and nothing else.
func (b *Bus) On[E any](fn func(context.Context, E)) (cancel func()) {
	key := reflect.TypeFor[E]()
	sub := &subscription{fn: fn, jobs: make(chan func(), queueDepth)}

	// One goroutine per subscription, draining in order. This is what makes
	// delivery serial per subscriber without serializing across subscribers.
	go func() {
		for job := range sub.jobs {
			job()
		}
	}()

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	if b.subs[key] == nil {
		b.subs[key] = map[uint64]*subscription{}
	}
	b.subs[key][id] = sub
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[key], id)
			if len(b.subs[key]) == 0 {
				delete(b.subs, key)
			}
			b.mu.Unlock()
			// Closing after the map delete means Emit, which holds the read
			// lock while queueing, can never send on a closed channel. The
			// drain goroutine finishes whatever is already queued and exits.
			close(sub.jobs)
		})
	}
}

// Emit delivers e to every handler registered for its type and returns without
// waiting for them.
//
// A panic in a handler is logged and contained: a subscriber's bug must not take
// down the request that wrote the note.
func (b *Bus) Emit[E any](ctx context.Context, e E) {
	// The read lock is held across the queueing, not just the lookup, so a
	// concurrent cancel cannot close a queue out from under us.
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subs[reflect.TypeFor[E]()] {
		fn, ok := sub.fn.(func(context.Context, E))
		if !ok {
			continue
		}
		b.wg.Add(1)
		job := func() {
			defer b.wg.Done()
			defer b.recoverHandler(e)
			fn(ctx, e)
		}
		select {
		case sub.jobs <- job:
		default:
			// This subscriber is wedged. Drop rather than block the writer.
			b.wg.Done()
			b.dropped.Add(1)
			b.logger().Warn("event dropped: subscriber queue full",
				"event", reflect.TypeFor[E]().String(), "dropped_total", b.dropped.Load())
		}
	}
}

// Dropped reports how many events have been discarded because a subscriber
// could not keep up. Non-zero means something is wedged and worth looking at.
func (b *Bus) Dropped() int64 { return b.dropped.Load() }

// recoverHandler keeps one bad subscriber from bringing down the process.
func (b *Bus) recoverHandler(e any) {
	r := recover()
	if r == nil {
		return
	}
	b.logger().Error("event handler panicked",
		"event", reflect.TypeOf(e).String(),
		"panic", r,
		"stack", string(debug.Stack()))
}

func (b *Bus) logger() *slog.Logger {
	if b.Log != nil {
		return b.Log
	}
	return slog.Default()
}

// Wait blocks until every event queued so far has been handled. It is for tests
// and for shutdown, not for the request path.
func (b *Bus) Wait() { b.wg.Wait() }
