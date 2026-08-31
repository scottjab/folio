package watch_test

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/scottjab/tsnotes/internal/vault"
	"github.com/scottjab/tsnotes/internal/watch"
)

// These exercise real fsnotify against a real directory, so they use real time
// with generous timeouts. The interesting timing logic lives in Debouncer and is
// tested deterministically under synctest.

type sink struct {
	mu    sync.Mutex
	paths map[string]int
	ch    chan struct{}
}

func newSink() *sink {
	return &sink{paths: map[string]int{}, ch: make(chan struct{}, 64)}
}

func (s *sink) onChange(paths []string) {
	s.mu.Lock()
	for _, p := range paths {
		s.paths[p]++
	}
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// waitFor polls until cond holds or the deadline passes.
func (s *sink) waitFor(t *testing.T, what string, cond func(map[string]int) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		ok := cond(s.paths)
		snapshot := make(map[string]int, len(s.paths))
		for k, v := range s.paths {
			snapshot[k] = v
		}
		s.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-s.ch:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %v", what, snapshot)
		}
	}
}

func (s *sink) seen(path string) func(map[string]int) bool {
	return func(m map[string]int) bool { return m[path] > 0 }
}

func newWatched(t *testing.T) (*vault.Vault, *sink) {
	t.Helper()
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })

	s := newSink()
	w, err := watch.NewWatcher(v, 50*time.Millisecond, 500*time.Millisecond, nil, s.onChange)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return v, s
}

func TestWatcherSeesExternalCreate(t *testing.T) {
	v, s := newWatched(t)
	writeExternal(t, v.Dir(), "new.md", "# New\n")
	s.waitFor(t, "new.md", s.seen("new.md"))
}

func TestWatcherSeesExternalEdit(t *testing.T) {
	v, s := newWatched(t)
	writeExternal(t, v.Dir(), "a.md", "# A\n")
	s.waitFor(t, "a.md create", s.seen("a.md"))

	writeExternal(t, v.Dir(), "a.md", "# A edited\n")
	s.waitFor(t, "a.md edit", func(m map[string]int) bool { return m["a.md"] >= 2 })
}

func TestWatcherSeesExternalDelete(t *testing.T) {
	v, s := newWatched(t)
	writeExternal(t, v.Dir(), "a.md", "# A\n")
	s.waitFor(t, "a.md create", s.seen("a.md"))

	if err := os.Remove(filepath.Join(v.Dir(), "a.md")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	s.waitFor(t, "a.md delete", func(m map[string]int) bool { return m["a.md"] >= 2 })
}

func TestWatcherIgnoresHiddenDirectories(t *testing.T) {
	// Obsidian rewrites .obsidian/workspace.json constantly. If that reached the
	// indexer we would reindex on every pane you drag.
	v, s := newWatched(t)
	mkdirExternal(t, v.Dir(), ".obsidian")
	writeExternal(t, v.Dir(), ".obsidian/workspace.json", "{}")
	writeExternal(t, v.Dir(), ".obsidian/notes.md", "# not content\n")

	// Use a real note as a fence: once it lands, the hidden writes have had
	// their chance too.
	writeExternal(t, v.Dir(), "real.md", "# Real\n")
	s.waitFor(t, "real.md", s.seen("real.md"))

	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.paths {
		if p != "real.md" {
			t.Errorf("watcher reported %q; hidden paths must never be reported", p)
		}
	}
}

func TestWatcherIgnoresNonMarkdown(t *testing.T) {
	v, s := newWatched(t)
	writeExternal(t, v.Dir(), "image.png", "notreallyapng")
	writeExternal(t, v.Dir(), "real.md", "# Real\n")
	s.waitFor(t, "real.md", s.seen("real.md"))

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.paths["image.png"]; ok {
		t.Error("watcher reported a non-markdown file")
	}
}

func TestWatcherPicksUpNewDirectories(t *testing.T) {
	// A git pull or an mkdir in Obsidian creates a folder we were not watching.
	v, s := newWatched(t)
	mkdirExternal(t, v.Dir(), "Projects/Deep")
	writeExternal(t, v.Dir(), "Projects/Deep/note.md", "# Deep\n")
	s.waitFor(t, "Projects/Deep/note.md", s.seen("Projects/Deep/note.md"))
}

func TestWatcherCoalescesAFlurry(t *testing.T) {
	v, s := newWatched(t)
	for range 10 {
		writeExternal(t, v.Dir(), "busy.md", "# Busy\n")
	}
	s.waitFor(t, "busy.md", s.seen("busy.md"))
	time.Sleep(300 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths["busy.md"] > 3 {
		t.Errorf("ten rapid writes produced %d batches; coalescing is not working", s.paths["busy.md"])
	}
}

func TestWatcherCloseIsIdempotent(t *testing.T) {
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	defer v.Close()

	w, err := watch.NewWatcher(v, 10*time.Millisecond, time.Second, nil, func([]string) {})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	w.Close() // must not panic or hang
}

func TestWatcherStopsReportingAfterClose(t *testing.T) {
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	defer v.Close()

	s := newSink()
	w, err := watch.NewWatcher(v, 10*time.Millisecond, time.Second, nil, s.onChange)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.Close()

	writeExternal(t, v.Dir(), "after.md", "# After\n")
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) != 0 {
		t.Errorf("watcher reported %v after Close", slices.Collect(maps(s.paths)))
	}
}

func maps(m map[string]int) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func writeExternal(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", rel, err)
	}
}

func mkdirExternal(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", rel, err)
	}
}
