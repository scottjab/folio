package watch_test

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/scottjab/folio/internal/watch"
)

// collector gathers flushed batches for assertions.
type collector struct {
	mu      sync.Mutex
	batches [][]string
}

func (c *collector) flush(paths []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch := slices.Clone(paths)
	slices.Sort(batch)
	c.batches = append(c.batches, batch)
}

func (c *collector) get() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.batches)
}

func TestCoalescesRepeatedEventsForOnePath(t *testing.T) {
	// A single editor save produces several filesystem events. The indexer must
	// see one.
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(100*time.Millisecond, time.Second, c.flush)
		defer d.Close()

		for range 5 {
			d.Add("a.md")
			synctest.Sleep(10 * time.Millisecond)
		}
		synctest.Sleep(200 * time.Millisecond)

		want := [][]string{{"a.md"}}
		if got := c.get(); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("batches = %v, want %v", got, want)
		}
	})
}

func TestBatchesDistinctPathsTogether(t *testing.T) {
	// A git pull touches many files at once; one batch means one index pass.
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(100*time.Millisecond, time.Second, c.flush)
		defer d.Close()

		d.Add("a.md")
		d.Add("b.md")
		d.Add("a.md")
		synctest.Sleep(200 * time.Millisecond)

		want := [][]string{{"a.md", "b.md"}}
		if got := c.get(); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("batches = %v, want one batch with both paths", got)
		}
	})
}

func TestQuietPeriodIsRespected(t *testing.T) {
	// Nothing should flush before the quiet period elapses.
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(100*time.Millisecond, time.Second, c.flush)
		defer d.Close()

		d.Add("a.md")
		synctest.Sleep(90 * time.Millisecond)
		if got := c.get(); len(got) != 0 {
			t.Fatalf("flushed early: %v", got)
		}

		synctest.Sleep(20 * time.Millisecond)
		if got := c.get(); len(got) != 1 {
			t.Errorf("batches = %v, want one flush just after the quiet period", got)
		}
	})
}

func TestSeparateBurstsFlushSeparately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(100*time.Millisecond, time.Second, c.flush)
		defer d.Close()

		d.Add("a.md")
		synctest.Sleep(200 * time.Millisecond)
		d.Add("b.md")
		synctest.Sleep(200 * time.Millisecond)

		want := [][]string{{"a.md"}, {"b.md"}}
		if got := c.get(); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("batches = %v, want %v", got, want)
		}
	})
}

func TestMaxDelayFlushesUnderAConstantStream(t *testing.T) {
	// Someone holding down a key, or a sync tool rewriting a file every 50ms,
	// would reset the quiet timer forever. The max delay is what stops a note
	// from never being indexed.
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(100*time.Millisecond, 500*time.Millisecond, c.flush)
		defer d.Close()

		// Stream for 1.2s, never quiet for longer than 50ms.
		for range 24 {
			d.Add("a.md")
			synctest.Sleep(50 * time.Millisecond)
		}

		got := c.get()
		if len(got) < 2 {
			t.Errorf("batches = %v, want at least two forced flushes over 1.2s with a 500ms max", got)
		}
		for _, b := range got {
			if !slices.Equal(b, []string{"a.md"}) {
				t.Errorf("batch = %v, want [a.md]", b)
			}
		}
	})
}

func TestMaxDelayIsMeasuredFromTheFirstEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(time.Second, 300*time.Millisecond, c.flush)
		defer d.Close()

		d.Add("a.md")
		// The quiet period is 1s and would not have fired yet, but the max delay
		// is 300ms and must.
		synctest.Sleep(350 * time.Millisecond)

		if got := c.get(); len(got) != 1 {
			t.Errorf("batches = %v, want the max delay to force a flush", got)
		}
	})
}

func TestCloseFlushesPendingWork(t *testing.T) {
	// Shutting down must not silently discard an edit that arrived a moment ago.
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(time.Second, 10*time.Second, c.flush)

		d.Add("a.md")
		d.Add("b.md")
		d.Close()

		want := [][]string{{"a.md", "b.md"}}
		if got := c.get(); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("batches = %v, want the pending work flushed on Close", got)
		}
	})
}

func TestCloseWithNothingPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(time.Second, 10*time.Second, c.flush)
		d.Close()
		if got := c.get(); len(got) != 0 {
			t.Errorf("batches = %v, want no empty flush", got)
		}
	})
}

func TestCloseIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := watch.NewDebouncer(time.Second, 10*time.Second, func([]string) {})
		d.Close()
		d.Close() // must not panic
	})
}

func TestAddAfterCloseIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(10*time.Millisecond, time.Second, c.flush)
		d.Close()
		d.Add("a.md")
		synctest.Sleep(100 * time.Millisecond)

		if got := c.get(); len(got) != 0 {
			t.Errorf("batches = %v, want adds after Close to be ignored", got)
		}
	})
}

func TestNoFlushWithoutEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(50*time.Millisecond, 100*time.Millisecond, c.flush)
		defer d.Close()

		synctest.Sleep(time.Second)
		if got := c.get(); len(got) != 0 {
			t.Errorf("batches = %v, want an idle debouncer to stay quiet", got)
		}
	})
}

func TestConcurrentAdds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &collector{}
		d := watch.NewDebouncer(50*time.Millisecond, time.Second, c.flush)
		defer d.Close()

		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.Add(string(rune('a'+i)) + ".md")
			}()
		}
		wg.Wait()
		synctest.Sleep(200 * time.Millisecond)

		got := c.get()
		total := 0
		for _, b := range got {
			total += len(b)
		}
		if total != 10 {
			t.Errorf("saw %d paths across %v, want all 10 exactly once", total, got)
		}
	})
}
