// Package watch keeps the index in step with files that change outside tsnotes:
// Obsidian on the desktop, a git pull, an rsync, a text editor.
//
// It is split in two on purpose. Debouncer holds all the timing logic and no I/O,
// so it can be tested deterministically under testing/synctest with a fake
// clock. Watcher is the thin fsnotify wiring around it, which needs a real
// filesystem and is tested against one.
package watch

import (
	"slices"
	"sync"
	"time"
)

// Debouncer coalesces a storm of filesystem events into batches.
//
// Two timers, because one is not enough:
//
//   - quiet is the settle time. Saving a file in most editors produces a create,
//     one or more writes, a chmod, and a rename; the indexer should see one
//     change, not five.
//   - maxDelay is the ceiling. A tool that rewrites a file every 50ms would reset
//     the quiet timer forever and the note would never be indexed. The ceiling
//     is measured from the first event in a batch, so a batch always lands.
type Debouncer struct {
	quiet    time.Duration
	maxDelay time.Duration
	flush    func(paths []string)

	mu      sync.Mutex
	pending map[string]struct{}
	// firstAt is when the current batch started, which is what maxDelay is
	// measured against.
	firstAt time.Time
	timer   *time.Timer
	closed  bool

	wg sync.WaitGroup
}

// NewDebouncer returns a Debouncer that calls flush with the set of distinct
// paths in each batch.
//
// flush runs on the Debouncer's own goroutine, one batch at a time, so it may
// take as long as it needs without dropping events; new events simply land in
// the next batch.
func NewDebouncer(quiet, maxDelay time.Duration, flush func(paths []string)) *Debouncer {
	return &Debouncer{
		quiet:    quiet,
		maxDelay: maxDelay,
		flush:    flush,
		pending:  map[string]struct{}{},
	}
}

// Add records that path changed. It is safe to call from any goroutine and
// returns immediately.
func (d *Debouncer) Add(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}

	if len(d.pending) == 0 {
		d.firstAt = time.Now()
	}
	d.pending[path] = struct{}{}
	d.rearmLocked()
}

// rearmLocked sets the timer to whichever deadline comes first: the quiet period
// from now, or the max delay from the start of this batch.
func (d *Debouncer) rearmLocked() {
	wait := d.quiet
	if deadline := d.firstAt.Add(d.maxDelay); time.Now().Add(wait).After(deadline) {
		wait = max(time.Until(deadline), 0)
	}

	if d.timer == nil {
		d.timer = time.AfterFunc(wait, d.fire)
		return
	}
	d.timer.Reset(wait)
}

// fire is the timer callback: take the batch and hand it to flush.
func (d *Debouncer) fire() {
	d.mu.Lock()
	batch := d.takeLocked()
	d.mu.Unlock()

	if len(batch) > 0 {
		d.flush(batch)
	}
}

// takeLocked drains the pending set. The caller holds the mutex.
func (d *Debouncer) takeLocked() []string {
	if len(d.pending) == 0 {
		return nil
	}
	batch := make([]string, 0, len(d.pending))
	for p := range d.pending {
		batch = append(batch, p)
	}
	clear(d.pending)
	d.firstAt = time.Time{}
	// Sorted so batches are deterministic, which makes both the logs and the
	// tests easier to read.
	slices.Sort(batch)
	return batch
}

// Close stops the debouncer, flushing whatever is pending first. An edit that
// arrived a moment before shutdown is still an edit. It is safe to call more
// than once.
func (d *Debouncer) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	if d.timer != nil {
		d.timer.Stop()
	}
	batch := d.takeLocked()
	d.mu.Unlock()

	if len(batch) > 0 {
		d.flush(batch)
	}
	d.wg.Wait()
}
