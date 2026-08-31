package vault_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scottjab/tsnotes/internal/vault"
)

func TestSetOpensAndCaches(t *testing.T) {
	s := vault.NewSet(t.TempDir())
	defer s.Close()

	a, err := s.Get("alice-github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, err := s.Get("alice-github")
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if a != b {
		t.Error("Set returned two handles for the same vault; they must be shared")
	}
}

func TestSetIsolatesVaults(t *testing.T) {
	root := t.TempDir()
	s := vault.NewSet(root)
	defer s.Close()

	alice, err := s.Get("alice-github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bob, err := s.Get("bob-github")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if _, err := alice.Write("secret.md", []byte("alice only"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if bob.Exists("secret.md") {
		t.Error("bob can see alice's note; the vaults are not isolated")
	}
	if _, err := os.Stat(filepath.Join(root, "alice-github", "secret.md")); err != nil {
		t.Errorf("note not where expected: %v", err)
	}
}

func TestSetRejectsEscapingDirNames(t *testing.T) {
	s := vault.NewSet(t.TempDir())
	defer s.Close()

	for _, dir := range []string{"../escape", "a/b", "", ".", "..", "/abs"} {
		if _, err := s.Get(dir); err == nil {
			t.Errorf("Get(%q) succeeded; a vault dir must be a single safe segment", dir)
		}
	}
}

func TestSetConcurrentGet(t *testing.T) {
	s := vault.NewSet(t.TempDir())
	defer s.Close()

	var wg sync.WaitGroup
	handles := make([]*vault.Vault, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.Get("shared")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			handles[i] = v
		}()
	}
	wg.Wait()

	for i, h := range handles {
		if h != handles[0] {
			t.Fatalf("handle %d differs; concurrent Get must return one shared vault", i)
		}
	}
}

func TestSetCloseReleasesEverything(t *testing.T) {
	s := vault.NewSet(t.TempDir())
	if _, err := s.Get("a"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.Get("b"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := s.Get("c"); err == nil {
		t.Error("Get after Close should fail")
	}
}
