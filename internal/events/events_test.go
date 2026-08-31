package events_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scottjab/folio/internal/events"
)

type noteChanged struct{ Path string }
type indexRebuilt struct{ Count int }

func TestOnAndEmit(t *testing.T) {
	b := events.NewBus()
	var got []string
	var mu sync.Mutex

	b.On(func(_ context.Context, e noteChanged) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e.Path)
	})

	b.Emit(context.Background(), noteChanged{Path: "a.md"})
	b.Emit(context.Background(), noteChanged{Path: "b.md"})
	b.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, []string{"a.md", "b.md"}) {
		t.Errorf("got %v, want both events in order", got)
	}
}

func TestEventsAreRoutedByType(t *testing.T) {
	b := events.NewBus()
	var notes, rebuilds atomic.Int64

	b.On(func(context.Context, noteChanged) { notes.Add(1) })
	b.On(func(context.Context, indexRebuilt) { rebuilds.Add(1) })

	b.Emit(context.Background(), noteChanged{Path: "a.md"})
	b.Wait()

	if notes.Load() != 1 {
		t.Errorf("noteChanged handler ran %d times, want 1", notes.Load())
	}
	if rebuilds.Load() != 0 {
		t.Errorf("indexRebuilt handler ran %d times; events crossed types", rebuilds.Load())
	}
}

func TestMultipleSubscribers(t *testing.T) {
	b := events.NewBus()
	var count atomic.Int64
	for range 3 {
		b.On(func(context.Context, noteChanged) { count.Add(1) })
	}

	b.Emit(context.Background(), noteChanged{})
	b.Wait()

	if count.Load() != 3 {
		t.Errorf("handlers ran %d times, want 3", count.Load())
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	b := events.NewBus()
	var count atomic.Int64
	cancel := b.On(func(context.Context, noteChanged) { count.Add(1) })

	b.Emit(context.Background(), noteChanged{})
	b.Wait()
	cancel()
	b.Emit(context.Background(), noteChanged{})
	b.Wait()

	if count.Load() != 1 {
		t.Errorf("handler ran %d times, want 1; cancel did not unsubscribe", count.Load())
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	b := events.NewBus()
	cancel := b.On(func(context.Context, noteChanged) {})
	cancel()
	cancel() // must not panic or remove someone else's subscription
}

func TestEmitWithNoSubscribers(t *testing.T) {
	b := events.NewBus()
	b.Emit(context.Background(), noteChanged{Path: "a.md"}) // must not panic
	b.Wait()
}

func TestPanickingHandlerIsContained(t *testing.T) {
	// One bad subscriber must not take down the writer that emitted the event,
	// nor stop the other subscribers from seeing it.
	b := events.NewBus()
	var survived atomic.Int64

	b.On(func(context.Context, noteChanged) { panic("boom") })
	b.On(func(context.Context, noteChanged) { survived.Add(1) })

	b.Emit(context.Background(), noteChanged{})
	b.Wait()

	if survived.Load() != 1 {
		t.Errorf("the second handler ran %d times, want 1", survived.Load())
	}
}

func TestConcurrentUse(t *testing.T) {
	// The bus is shared by HTTP handlers, the file watcher, and the MCP server,
	// so subscribing while emitting has to be safe.
	b := events.NewBus()
	var wg sync.WaitGroup
	var seen atomic.Int64

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				cancel := b.On(func(context.Context, noteChanged) { seen.Add(1) })
				b.Emit(context.Background(), noteChanged{Path: "x"})
				cancel()
			}
		}()
	}
	wg.Wait()
	b.Wait()
}

func TestHandlerSeesTheEmitContext(t *testing.T) {
	b := events.NewBus()
	type key struct{}
	got := make(chan any, 1)

	b.On(func(ctx context.Context, _ noteChanged) { got <- ctx.Value(key{}) })
	b.Emit(context.WithValue(context.Background(), key{}, "carried"), noteChanged{})
	b.Wait()

	if v := <-got; v != "carried" {
		t.Errorf("context value = %v, want it carried through to the handler", v)
	}
}
