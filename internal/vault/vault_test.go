package vault_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scottjab/folio/internal/vault"
	"github.com/scottjab/folio/internal/vaultpath"
)

var fixedTime = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func newVault(t *testing.T) *vault.Vault {
	t.Helper()
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v.Now = func() time.Time { return fixedTime }
	t.Cleanup(func() { v.Close() })
	return v
}

func TestOpenCreatesLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "vault")
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer v.Close()

	for _, sub := range []string{".folio/tmp", ".folio/trash"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Errorf("expected %s to exist as a directory: %v", sub, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	v1, err := vault.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := v1.Write("a.md", []byte("hi"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	v1.Close()

	v2, err := vault.Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer v2.Close()
	content, _, err := v2.Read("a.md")
	if err != nil || string(content) != "hi" {
		t.Errorf("reopened vault lost data: %q %v", content, err)
	}
}

func TestRoundTrip(t *testing.T) {
	v := newVault(t)

	n, err := v.Write("Daily/2026-08-30.md", []byte("# Hi\n"), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n.Path != "Daily/2026-08-30.md" {
		t.Errorf("Path = %q", n.Path)
	}
	if n.SHA256 == "" {
		t.Error("SHA256 is empty")
	}
	if n.Size != 5 {
		t.Errorf("Size = %d, want 5", n.Size)
	}

	got, n2, err := v.Read("Daily/2026-08-30.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "# Hi\n" {
		t.Errorf("Read = %q", got)
	}
	if n2.SHA256 != n.SHA256 {
		t.Errorf("sha changed across write/read: %q vs %q", n.SHA256, n2.SHA256)
	}

	if _, err := v.Write("Daily/2026-08-30.md", []byte("# Bye\n"), n.SHA256); err != nil {
		t.Fatalf("update with matching base sha: %v", err)
	}
	got, _, _ = v.Read("Daily/2026-08-30.md")
	if string(got) != "# Bye\n" {
		t.Errorf("after update Read = %q", got)
	}

	if err := v.Delete("Daily/2026-08-30.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := v.Read("Daily/2026-08-30.md"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Read after delete: %v, want ErrNotFound", err)
	}
}

func TestWriteCreatesParentDirs(t *testing.T) {
	v := newVault(t)
	if _, err := v.Write("a/b/c/deep.md", []byte("x"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, _, err := v.Read("a/b/c/deep.md"); err != nil {
		t.Errorf("Read: %v", err)
	}
}

func TestReadMissing(t *testing.T) {
	v := newVault(t)
	if _, _, err := v.Read("nope.md"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Read missing = %v, want ErrNotFound", err)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	v := newVault(t)
	if _, err := v.Create("a.md", []byte("first")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := v.Create("a.md", []byte("second")); !errors.Is(err, vault.ErrExists) {
		t.Errorf("second Create = %v, want ErrExists", err)
	}
	got, _, _ := v.Read("a.md")
	if string(got) != "first" {
		t.Errorf("content = %q, want the original to survive", got)
	}
}

func TestStaleCASWritesConflictFile(t *testing.T) {
	v := newVault(t)

	first, err := v.Write("Note.md", []byte("original\n"), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Someone else (Obsidian, say) writes underneath us.
	if _, err := v.Write("Note.md", []byte("theirs\n"), ""); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	_, err = v.Write("Note.md", []byte("mine\n"), first.SHA256)
	if !errors.Is(err, vault.ErrConflict) {
		t.Fatalf("stale CAS write = %v, want ErrConflict", err)
	}

	// Their version must still be the one at the real path.
	got, _, _ := v.Read("Note.md")
	if string(got) != "theirs\n" {
		t.Errorf("Note.md = %q, want the other writer's content preserved", got)
	}

	// Our version must not be lost. It lands beside the note.
	want := "Note (conflict 2026-08-30T12-00-00Z).md"
	side, _, err := v.Read(want)
	if err != nil {
		entries, _ := v.List()
		t.Fatalf("expected conflict file %q: %v (vault has %+v)", want, err, entries)
	}
	if string(side) != "mine\n" {
		t.Errorf("conflict file = %q, want our rejected content", side)
	}

	var ce *vault.ConflictError
	if !errors.As(err2(v, first.SHA256), &ce) {
		t.Fatal("error should carry a *ConflictError with the details")
	}
}

func err2(v *vault.Vault, stale string) error {
	_, err := v.Write("Note.md", []byte("mine again\n"), stale)
	return err
}

func TestCASAgainstMissingFile(t *testing.T) {
	v := newVault(t)
	// Claiming to update a note that no longer exists is a conflict, not a
	// silent recreate: the note may have been deleted deliberately.
	if _, err := v.Write("gone.md", []byte("x"), "deadbeef"); !errors.Is(err, vault.ErrConflict) {
		t.Errorf("CAS on missing file = %v, want ErrConflict", err)
	}
}

func TestDeleteMovesToTrash(t *testing.T) {
	v := newVault(t)
	if _, err := v.Write("Daily/x.md", []byte("keepme"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := v.Delete("Daily/x.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	trash := filepath.Join(v.Dir(), ".folio", "trash")
	var found string
	filepath.WalkDir(trash, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("delete did not leave anything in .folio/trash")
	}
	if !strings.Contains(found, "2026-08-30T12-00-00Z") {
		t.Errorf("trash entry %q is not timestamped", found)
	}
	b, _ := os.ReadFile(found)
	if string(b) != "keepme" {
		t.Errorf("trashed content = %q, want it intact", b)
	}
}

func TestMove(t *testing.T) {
	v := newVault(t)
	if _, err := v.Write("a.md", []byte("body"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := v.Move("a.md", "Archive/b.md"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, _, err := v.Read("a.md"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("source still readable after move: %v", err)
	}
	got, _, err := v.Read("Archive/b.md")
	if err != nil || string(got) != "body" {
		t.Errorf("destination = %q, %v", got, err)
	}
}

func TestMoveRefusesToClobber(t *testing.T) {
	v := newVault(t)
	v.Write("a.md", []byte("a"), "")
	v.Write("b.md", []byte("b"), "")
	if err := v.Move("a.md", "b.md"); !errors.Is(err, vault.ErrExists) {
		t.Errorf("Move onto an existing note = %v, want ErrExists", err)
	}
	got, _, _ := v.Read("b.md")
	if string(got) != "b" {
		t.Errorf("destination was clobbered: %q", got)
	}
}

func TestList(t *testing.T) {
	v := newVault(t)
	for _, p := range []string{"a.md", "Daily/b.md", "attachments/img.png", "notes.txt"} {
		if _, err := v.Write(p, []byte("x"), ""); err != nil {
			t.Fatalf("Write(%q): %v", p, err)
		}
	}
	// Things that must never show up in a listing.
	mustMkdir(t, v.Dir(), ".obsidian")
	mustWrite(t, v.Dir(), ".obsidian/app.json", "{}")
	mustMkdir(t, v.Dir(), ".git")
	mustWrite(t, v.Dir(), ".git/config", "[core]")
	mustWrite(t, v.Dir(), ".folio/tmp/leftover", "junk")

	entries, err := v.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path)
	}
	slices.Sort(got)
	want := []string{"Daily/b.md", "a.md", "attachments/img.png", "notes.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

func TestListNotes(t *testing.T) {
	v := newVault(t)
	for _, p := range []string{"a.md", "b.markdown", "img.png", "notes.txt"} {
		v.Write(p, []byte("x"), "")
	}
	notes, err := v.ListNotes()
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	slices.Sort(notes)
	if !slices.Equal(notes, []string{"a.md", "b.markdown"}) {
		t.Errorf("ListNotes = %v, want only markdown", notes)
	}
}

func TestPathEscapeIsRefused(t *testing.T) {
	v := newVault(t)
	for _, p := range []string{"../escape.md", "a/../../b.md", ".folio/tmp/x", "", "."} {
		if _, err := v.Write(p, []byte("x"), ""); !errors.Is(err, vaultpath.ErrInvalidPath) {
			t.Errorf("Write(%q) = %v, want ErrInvalidPath", p, err)
		}
		if _, _, err := v.Read(p); !errors.Is(err, vaultpath.ErrInvalidPath) {
			t.Errorf("Read(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestAbsoluteLookingPathIsConfinedNotEscaped(t *testing.T) {
	// A leading slash means "from the vault root", the same way Obsidian reads
	// it in a wikilink. It must never reach the real filesystem root.
	v := newVault(t)
	n, err := v.Write("/etc/passwd", []byte("harmless"), "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n.Path != "etc/passwd" {
		t.Errorf("Path = %q, want it rebased onto the vault root", n.Path)
	}
	if _, err := os.Stat(filepath.Join(v.Dir(), "etc", "passwd")); err != nil {
		t.Errorf("expected the file inside the vault: %v", err)
	}

	real, err := os.ReadFile("/etc/passwd")
	if err == nil && bytes.Contains(real, []byte("harmless")) {
		t.Fatal("clobbered the real /etc/passwd")
	}
}

func TestSymlinkEscapeIsRefused(t *testing.T) {
	v := newVault(t)
	secret := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// A symlink planted inside the vault, exactly what an attacker with write
	// access to the state dir, or a careless rsync, would leave behind.
	if err := os.Symlink(secret, filepath.Join(v.Dir(), "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, _, err := v.Read("link.md")
	if err == nil {
		t.Fatalf("Read through an escaping symlink succeeded and returned %q", got)
	}
	if bytes.Contains(got, []byte("classified")) {
		t.Fatal("leaked content from outside the vault")
	}

	// It must not appear in listings either.
	entries, _ := v.List()
	for _, e := range entries {
		if e.Path == "link.md" {
			t.Error("escaping symlink showed up in List")
		}
	}
}

func TestWritesAreAtomic(t *testing.T) {
	// A reader must only ever observe a complete version of the file, never a
	// half-written one. We hammer a large note from both sides.
	v := newVault(t)
	old := bytes.Repeat([]byte("a"), 512*1024)
	newer := bytes.Repeat([]byte("b"), 512*1024)
	if _, err := v.Write("big.md", old, ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 40 {
			v.Write("big.md", newer, "")
			v.Write("big.md", old, "")
		}
		close(stop)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, _, err := v.Read("big.md")
			if err != nil {
				continue
			}
			if !bytes.Equal(got, old) && !bytes.Equal(got, newer) {
				t.Errorf("observed a torn read: %d bytes, prefix %q", len(got), got[:min(16, len(got))])
				return
			}
		}
	}()
	wg.Wait()

	// The staging area must not be left full of debris.
	tmp, _ := os.ReadDir(filepath.Join(v.Dir(), ".folio", "tmp"))
	if len(tmp) != 0 {
		t.Errorf("%d temp files left behind in .folio/tmp", len(tmp))
	}
}

func TestConcurrentWritesToSamePathSerialize(t *testing.T) {
	v := newVault(t)
	if _, err := v.Write("counter.md", []byte("0"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}

	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.Write("counter.md", []byte(strings.Repeat("x", i+1)), "")
		}()
	}
	wg.Wait()

	got, n, err := v.Read("counter.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Whoever won, the file must be exactly one writer's payload, and its
	// recorded size must match its content.
	if len(got) < 1 || len(got) > writers || strings.Trim(string(got), "x") != "" {
		t.Errorf("content = %q, want one writer's complete payload", got)
	}
	if n.Size != int64(len(got)) {
		t.Errorf("Size = %d, content is %d bytes", n.Size, len(got))
	}
}

func TestStat(t *testing.T) {
	v := newVault(t)
	written, _ := v.Write("a.md", []byte("hello"), "")
	got, err := v.Stat("a.md")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got.SHA256 != written.SHA256 || got.Size != written.Size {
		t.Errorf("Stat = %+v, want to match the write %+v", got, written)
	}
	if _, err := v.Stat("nope.md"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Stat missing = %v, want ErrNotFound", err)
	}
}

func TestExists(t *testing.T) {
	v := newVault(t)
	v.Write("a.md", []byte("x"), "")
	if !v.Exists("a.md") {
		t.Error("Exists(a.md) = false")
	}
	if v.Exists("b.md") {
		t.Error("Exists(b.md) = true")
	}
	if v.Exists("../outside.md") {
		t.Error("Exists on an invalid path must be false, not an escape")
	}
}

func mustMkdir(t *testing.T, base, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, rel), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", rel, err)
	}
}

func mustWrite(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", rel, err)
	}
}
