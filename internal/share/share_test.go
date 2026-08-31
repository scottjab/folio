package share_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
)

type env struct {
	res   *share.Resolver
	db    *store.DB
	alice identity.User
	bob   identity.User
	carol identity.User
	ctx   context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	mk := func(id int64, login string) identity.User {
		t.Helper()
		if _, err := db.Exec(ctx,
			`INSERT INTO users (tailscale_user_id, login, display_name, vault_dir, created_at) VALUES (?,?,?,?,0)`,
			id, login, login, login); err != nil {
			t.Fatalf("seed %s: %v", login, err)
		}
		uid, _ := db.One[int64](ctx, `SELECT id FROM users WHERE login = ?`, login)
		if _, err := db.Exec(ctx, `INSERT INTO vaults (user_id, dir, created_at) VALUES (?,?,0)`, uid, login); err != nil {
			t.Fatalf("seed vault %s: %v", login, err)
		}
		vid, _ := db.One[int64](ctx, `SELECT id FROM vaults WHERE user_id = ?`, uid)
		return identity.User{ID: uid, Login: login, VaultID: vid, VaultDir: login}
	}

	return &env{
		res:   share.NewResolver(db),
		db:    db,
		alice: mk(1, "alice@github"),
		bob:   mk(2, "bob@github"),
		carol: mk(3, "carol@github"),
		ctx:   ctx,
	}
}

func TestOwnerHasFullAccess(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"a.md", "Daily/x.md", "deep/nested/note.md"} {
		for _, perm := range []share.Perm{share.Read, share.Write} {
			if err := e.res.Check(e.ctx, e.alice, e.alice.VaultID, p, perm); err != nil {
				t.Errorf("owner denied %s on %q: %v", perm, p, err)
			}
		}
	}
}

func TestStrangerHasNoAccess(t *testing.T) {
	e := newEnv(t)
	err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "a.md", share.Read)
	if !errors.Is(err, share.ErrDenied) {
		t.Errorf("Check = %v, want ErrDenied", err)
	}
}

func TestGrantOnASingleNote(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Daily/x.md", false, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "Daily/x.md", share.Read); err != nil {
		t.Errorf("grantee denied read: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "Daily/x.md", share.Write); !errors.Is(err, share.ErrDenied) {
		t.Errorf("read grant allowed a write: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "Daily/y.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("a grant on one note leaked to another: %v", err)
	}
	if err := e.res.Check(e.ctx, e.carol, e.alice.VaultID, "Daily/x.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("a grant to bob leaked to carol: %v", err)
	}
}

func TestWriteImpliesRead(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Write); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "a.md", share.Read); err != nil {
		t.Errorf("write grant did not imply read: %v", err)
	}
}

func TestFolderGrantCoversDescendants(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects", true, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	for _, p := range []string{"Projects/a.md", "Projects/sub/deep/b.md", "Projects"} {
		if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, p, share.Read); err != nil {
			t.Errorf("folder grant did not cover %q: %v", p, err)
		}
	}
}

func TestFolderGrantIsNotAStringPrefix(t *testing.T) {
	// The classic bug: "Projects" must not grant access to "Projects2".
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects", true, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	for _, p := range []string{"Projects2/secret.md", "ProjectsOld/x.md", "Other/Projects/x.md"} {
		if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, p, share.Read); !errors.Is(err, share.ErrDenied) {
			t.Errorf("folder grant on Projects leaked to %q: %v", p, err)
		}
	}
}

func TestDeepestGrantWins(t *testing.T) {
	// A broad read grant plus a narrow write grant inside it should give write
	// on the narrow part, which is how anyone would expect it to behave.
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects", true, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects/shared", true, "bob@github", share.Write); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "Projects/shared/a.md", share.Write); err != nil {
		t.Errorf("the deeper write grant did not apply: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "Projects/other.md", share.Write); !errors.Is(err, share.ErrDenied) {
		t.Errorf("the deeper write grant leaked upward: %v", err)
	}
}

func TestNoteGrantBeatsFolderGrant(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects", true, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects/a.md", false, "bob@github", share.Write); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "Projects/a.md", share.Write); err != nil {
		t.Errorf("the exact-note grant did not win: %v", err)
	}
}

func TestRevokeTakesEffectImmediately(t *testing.T) {
	e := newEnv(t)
	s, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Read)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "a.md", share.Read); err != nil {
		t.Fatalf("grant did not apply: %v", err)
	}

	if err := e.res.Revoke(e.ctx, e.alice, s.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "a.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("access survived revocation: %v", err)
	}
}

func TestAGrantOnlyEverCoversTheGrantersOwnVault(t *testing.T) {
	// Grant takes no vault id: it always uses the caller's. That makes sharing
	// someone else's note structurally impossible rather than merely checked.
	// Bob sharing "a.md" hands out *his* a.md, never Alice's.
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.bob, "a.md", false, "carol@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := e.res.Check(e.ctx, e.carol, e.bob.VaultID, "a.md", share.Read); err != nil {
		t.Errorf("carol should be able to read bob's a.md: %v", err)
	}
	if err := e.res.Check(e.ctx, e.carol, e.alice.VaultID, "a.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("bob's grant reached into alice's vault: %v", err)
	}
}

func TestForgedVaultIDIsRefused(t *testing.T) {
	// The belt-and-braces case: a caller assembling an identity.User by hand
	// with someone else's vault id must not be able to grant out of it.
	e := newEnv(t)
	forged := e.bob
	forged.VaultID = e.alice.VaultID

	if _, err := e.res.Grant(e.ctx, forged, "a.md", false, "carol@github", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Fatalf("Grant with a forged vault id = %v, want ErrDenied", err)
	}
	if err := e.res.Check(e.ctx, e.carol, e.alice.VaultID, "a.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("carol gained access to alice's vault: %v", err)
	}
}

func TestCannotRevokeSomeoneElsesShare(t *testing.T) {
	e := newEnv(t)
	s, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Read)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := e.res.Revoke(e.ctx, e.bob, s.ID); err == nil {
		t.Fatal("bob revoked alice's share")
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "a.md", share.Read); err != nil {
		t.Errorf("the share should still stand: %v", err)
	}
}

func TestCannotShareWithYourself(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "alice@github", share.Read); err == nil {
		t.Error("sharing with yourself should be refused as a no-op mistake")
	}
}

func TestRegrantUpdatesThePermission(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Write); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "a.md", share.Write); err != nil {
		t.Errorf("upgrade to write did not apply: %v", err)
	}
	n, _ := e.db.One[int](e.ctx, `SELECT count(*) FROM shares`)
	if n != 1 {
		t.Errorf("shares rows = %d, want the grant updated rather than duplicated", n)
	}
}

func TestReadableVaults(t *testing.T) {
	e := newEnv(t)
	// Your own vault is always readable, even with no shares at all.
	ids, err := e.res.ReadableVaults(e.ctx, e.bob)
	if err != nil {
		t.Fatalf("ReadableVaults: %v", err)
	}
	if !slices.Equal(ids, []int64{e.bob.VaultID}) {
		t.Errorf("ReadableVaults = %v, want just bob's own vault", ids)
	}

	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	ids, err = e.res.ReadableVaults(e.ctx, e.bob)
	if err != nil {
		t.Fatalf("ReadableVaults: %v", err)
	}
	slices.Sort(ids)
	want := []int64{e.alice.VaultID, e.bob.VaultID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("ReadableVaults = %v, want %v", ids, want)
	}
}

func TestSharedWithMe(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Daily/x.md", false, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects", true, "carol@github", share.Write); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	got, err := e.res.SharedWithMe(e.ctx, e.bob)
	if err != nil {
		t.Fatalf("SharedWithMe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d shares, want 1: %+v", len(got), got)
	}
	if got[0].Path != "Daily/x.md" || got[0].OwnerLogin != "alice@github" {
		t.Errorf("share = %+v", got[0])
	}

	// Carol's grant must not show up in bob's list.
	for _, s := range got {
		if s.GranteeLogin != "bob@github" {
			t.Errorf("bob saw a share granted to %q", s.GranteeLogin)
		}
	}
}

func TestSharesIMade(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := e.res.Grant(e.ctx, e.bob, "b.md", false, "carol@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	got, err := e.res.SharesIMade(e.ctx, e.alice)
	if err != nil {
		t.Fatalf("SharesIMade: %v", err)
	}
	if len(got) != 1 || got[0].GranteeLogin != "bob@github" {
		t.Errorf("SharesIMade = %+v, want only alice's own grant", got)
	}
}

func TestGrantToUnknownUserIsRefused(t *testing.T) {
	// Silently accepting a typo'd login would leave you believing you had
	// shared a note when you had not.
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "typo@github", share.Read); err == nil {
		t.Error("granting to an unknown login should be refused")
	}
}

func TestGrantValidatesInputs(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "../escape.md", false, "bob@github", share.Read); err == nil {
		t.Error("an escaping path was accepted")
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "a.md", false, "bob@github", share.Perm("admin")); err == nil {
		t.Error("an invalid permission was accepted")
	}
}

func TestVisiblePathsFilter(t *testing.T) {
	// Used to trim search results to what a grantee may actually read.
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "Projects", true, "bob@github", share.Read); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	paths := []string{"Projects/a.md", "Projects/sub/b.md", "Projects2/c.md", "Private/d.md"}
	got, err := e.res.FilterReadable(e.ctx, e.bob, e.alice.VaultID, paths)
	if err != nil {
		t.Fatalf("FilterReadable: %v", err)
	}
	want := []string{"Projects/a.md", "Projects/sub/b.md"}
	if !slices.Equal(got, want) {
		t.Errorf("FilterReadable = %v, want %v", got, want)
	}

	// The owner sees everything, with no per-path queries needed.
	got, err = e.res.FilterReadable(e.ctx, e.alice, e.alice.VaultID, paths)
	if err != nil {
		t.Fatalf("FilterReadable(owner): %v", err)
	}
	if !slices.Equal(got, paths) {
		t.Errorf("owner FilterReadable = %v, want everything", got)
	}
}
