package identity_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/store"
)

func newResolver(t *testing.T, whois identity.WhoIsFunc, opts ...func(*identity.Options)) (*identity.Resolver, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	o := identity.Options{CacheTTL: 30 * time.Second}
	for _, fn := range opts {
		fn(&o)
	}
	return identity.NewResolver(db, whois, o), db
}

func staticWhoIs(w identity.WhoIs) identity.WhoIsFunc {
	return func(context.Context, string) (identity.WhoIs, error) { return w, nil }
}

var alice = identity.WhoIs{
	UserID:      101,
	Login:       "alice@github",
	DisplayName: "Alice",
	ProfilePic:  "https://example.com/a.png",
	NodeName:    "laptop",
}

func TestIdentifyCreatesUserAndVaultOnFirstSight(t *testing.T) {
	r, db := newResolver(t, staticWhoIs(alice))
	ctx := context.Background()

	u, err := r.Identify(ctx, "100.64.0.1:1234")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if u.Login != "alice@github" || u.DisplayName != "Alice" {
		t.Errorf("user = %+v", u)
	}
	if u.VaultDir != "alice-github" {
		t.Errorf("VaultDir = %q, want the slugged login", u.VaultDir)
	}
	if u.ID == 0 || u.VaultID == 0 {
		t.Errorf("user = %+v, want both ids populated", u)
	}

	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 1 {
		t.Errorf("users rows = %d, want 1", n)
	}
	n, _ = db.One[int](ctx, `SELECT count(*) FROM vaults`)
	if n != 1 {
		t.Errorf("vaults rows = %d, want 1", n)
	}
}

func TestIdentifyIsIdempotent(t *testing.T) {
	r, db := newResolver(t, staticWhoIs(alice))
	ctx := context.Background()

	first, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	second, err := r.Identify(ctx, "100.64.0.2:2") // same user, different device
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if first.ID != second.ID || first.VaultID != second.VaultID {
		t.Errorf("same user got different records: %+v vs %+v", first, second)
	}
	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 1 {
		t.Errorf("users rows = %d, want 1 across two devices", n)
	}
}

func TestLoginRenameKeepsTheSameVault(t *testing.T) {
	// Changing your tailnet login must not orphan your notes. The tailscale user
	// id is the identity; the login is just a label.
	renamed := alice
	renamed.Login = "alice@passkey"
	renamed.DisplayName = "Alice Renamed"

	var current atomic.Pointer[identity.WhoIs]
	current.Store(&alice)
	r, db := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		return *current.Load(), nil
	})
	ctx := context.Background()

	before, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}

	current.Store(&renamed)
	r.Flush()

	after, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify after rename: %v", err)
	}
	if after.VaultDir != before.VaultDir {
		t.Errorf("VaultDir changed from %q to %q; the vault was orphaned", before.VaultDir, after.VaultDir)
	}
	if after.ID != before.ID {
		t.Errorf("user id changed from %d to %d", before.ID, after.ID)
	}
	if after.Login != "alice@passkey" {
		t.Errorf("Login = %q, want the new login recorded", after.Login)
	}
	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 1 {
		t.Errorf("users rows = %d, want the rename to update rather than insert", n)
	}
}

func TestDistinctUsersGetDistinctVaults(t *testing.T) {
	bob := identity.WhoIs{UserID: 202, Login: "bob@github", DisplayName: "Bob"}
	var who atomic.Pointer[identity.WhoIs]
	who.Store(&alice)

	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) { return *who.Load(), nil })
	ctx := context.Background()

	a, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	who.Store(&bob)
	r.Flush()
	b, err := r.Identify(ctx, "100.64.0.2:2")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}

	if a.VaultID == b.VaultID || a.VaultDir == b.VaultDir {
		t.Errorf("two users share a vault: %+v vs %+v", a, b)
	}
}

func TestVaultDirCollisionIsResolved(t *testing.T) {
	// Two different logins can slug to the same directory. The second must get
	// its own, not silently share the first one's notes.
	one := identity.WhoIs{UserID: 1, Login: "a.b@github"}
	two := identity.WhoIs{UserID: 2, Login: "a-b@github"}

	var who atomic.Pointer[identity.WhoIs]
	who.Store(&one)
	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) { return *who.Load(), nil })
	ctx := context.Background()

	u1, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	who.Store(&two)
	r.Flush()
	u2, err := r.Identify(ctx, "100.64.0.2:2")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}

	if u1.VaultDir == u2.VaultDir {
		t.Fatalf("both users got vault dir %q", u1.VaultDir)
	}
}

func TestTaggedNodeIsRefusedByDefault(t *testing.T) {
	// A tagged node has no human behind it. Guessing which user's notes it
	// should see would be exactly the wrong call.
	tagged := identity.WhoIs{Tags: []string{"tag:ci"}, NodeName: "build-server"}
	r, _ := newResolver(t, staticWhoIs(tagged))

	_, err := r.Identify(context.Background(), "100.64.0.9:1")
	if !errors.Is(err, identity.ErrNoIdentity) {
		t.Errorf("Identify(tagged) = %v, want ErrNoIdentity", err)
	}
}

func TestTaggedNodeActsAsAConfiguredUser(t *testing.T) {
	// An agent running on a tagged server is the reason this exists: you map the
	// tag to yourself and your agent sees your notes and nothing else.
	tagged := identity.WhoIs{Tags: []string{"tag:notes-agent"}, NodeName: "agent-box"}
	var who atomic.Pointer[identity.WhoIs]
	who.Store(&alice)

	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		return *who.Load(), nil
	}, func(o *identity.Options) {
		o.Agents = map[string]string{"tag:notes-agent": "alice@github"}
	})
	ctx := context.Background()

	// Alice has to exist before her agent can act as her; see
	// TestAgentCannotConjureAUser for why.
	if _, err := r.Identify(ctx, "100.64.0.1:1"); err != nil {
		t.Fatalf("Identify(alice): %v", err)
	}
	who.Store(&tagged)
	r.Flush()

	u, err := r.Identify(ctx, "100.64.0.9:1")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if u.Login != "alice@github" {
		t.Errorf("Login = %q, want the mapped user", u.Login)
	}
	if !u.IsAgent {
		t.Error("IsAgent = false, want the request marked as coming from an agent")
	}
	if u.AgentTag != "tag:notes-agent" {
		t.Errorf("AgentTag = %q", u.AgentTag)
	}
}

func TestAgentSharesTheVaultOfTheUserItActsAs(t *testing.T) {
	human := staticWhoIs(alice)
	agentWho := staticWhoIs(identity.WhoIs{Tags: []string{"tag:notes-agent"}, NodeName: "agent"})

	var mode atomic.Bool
	r, _ := newResolver(t, func(ctx context.Context, addr string) (identity.WhoIs, error) {
		if mode.Load() {
			return agentWho(ctx, addr)
		}
		return human(ctx, addr)
	}, func(o *identity.Options) {
		o.Agents = map[string]string{"tag:notes-agent": "alice@github"}
	})
	ctx := context.Background()

	person, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify(person): %v", err)
	}
	mode.Store(true)
	r.Flush()
	agent, err := r.Identify(ctx, "100.64.0.9:1")
	if err != nil {
		t.Fatalf("Identify(agent): %v", err)
	}

	if agent.VaultID != person.VaultID {
		t.Errorf("agent vault %d, person vault %d; the agent must see the same notes", agent.VaultID, person.VaultID)
	}
	if agent.ID != person.ID {
		t.Errorf("agent user id %d, person user id %d", agent.ID, person.ID)
	}
}

func TestAgentCannotConjureAUser(t *testing.T) {
	// A tagged node may borrow an identity, never mint one. If the mapping names
	// someone folio has never seen, that is a configuration mistake (a typo in
	// the login, most likely) and it should be loud rather than silently
	// creating an empty vault nobody can reach.
	tagged := identity.WhoIs{Tags: []string{"tag:notes-agent"}, NodeName: "agent-box"}
	r, db := newResolver(t, staticWhoIs(tagged), func(o *identity.Options) {
		o.Agents = map[string]string{"tag:notes-agent": "typo@github"}
	})
	ctx := context.Background()

	_, err := r.Identify(ctx, "100.64.0.9:1")
	if !errors.Is(err, identity.ErrNoIdentity) {
		t.Errorf("Identify = %v, want ErrNoIdentity", err)
	}
	if !errors.Is(err, identity.ErrUnknownUser) {
		t.Errorf("the error should say the mapped user is unknown: %v", err)
	}
	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 0 {
		t.Errorf("users rows = %d, want the agent to have created nobody", n)
	}
}

func TestUnknownAgentTagIsStillRefused(t *testing.T) {
	tagged := identity.WhoIs{Tags: []string{"tag:other"}}
	r, _ := newResolver(t, staticWhoIs(tagged), func(o *identity.Options) {
		o.Agents = map[string]string{"tag:notes-agent": "alice@github"}
	})
	if _, err := r.Identify(context.Background(), "100.64.0.9:1"); !errors.Is(err, identity.ErrNoIdentity) {
		t.Errorf("Identify = %v, want ErrNoIdentity for an unmapped tag", err)
	}
}

func TestWhoIsFailureIsUnavailableNotDenied(t *testing.T) {
	// tailscaled being briefly unreachable is a 503, not a 403. Telling someone
	// they are unauthorized when the identity service is down sends them off
	// debugging the wrong thing.
	boom := errors.New("localapi unreachable")
	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		return identity.WhoIs{}, boom
	})

	_, err := r.Identify(context.Background(), "100.64.0.1:1")
	if !errors.Is(err, identity.ErrUnavailable) {
		t.Errorf("Identify = %v, want ErrUnavailable", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying cause should be preserved: %v", err)
	}
}

func TestBadRemoteAddrIsRejected(t *testing.T) {
	r, _ := newResolver(t, staticWhoIs(alice))
	for _, addr := range []string{"", "garbage", "notanip:80"} {
		if _, err := r.Identify(context.Background(), addr); err == nil {
			t.Errorf("Identify(%q) succeeded, want an error", addr)
		}
	}
}

func TestCacheAvoidsRepeatedWhoIsCalls(t *testing.T) {
	var calls atomic.Int64
	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		calls.Add(1)
		return alice, nil
	})
	ctx := context.Background()

	for range 5 {
		if _, err := r.Identify(ctx, "100.64.0.1:1234"); err != nil {
			t.Fatalf("Identify: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("WhoIs called %d times, want 1; the cache is not working", calls.Load())
	}
}

func TestCacheIsKeyedByAddressNotPort(t *testing.T) {
	// Every HTTP connection from the same device arrives on a new source port.
	// Keying on the full remote address would make the cache useless.
	var calls atomic.Int64
	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		calls.Add(1)
		return alice, nil
	})
	ctx := context.Background()

	for _, port := range []string{"1", "2", "3"} {
		if _, err := r.Identify(ctx, "100.64.0.1:"+port); err != nil {
			t.Fatalf("Identify: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("WhoIs called %d times across three source ports, want 1", calls.Load())
	}
}

func TestCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer db.Close()

		r := identity.NewResolver(db, func(context.Context, string) (identity.WhoIs, error) {
			calls.Add(1)
			return alice, nil
		}, identity.Options{CacheTTL: time.Minute})
		ctx := context.Background()

		if _, err := r.Identify(ctx, "100.64.0.1:1"); err != nil {
			t.Fatalf("Identify: %v", err)
		}
		synctest.Sleep(30 * time.Second)
		if _, err := r.Identify(ctx, "100.64.0.1:1"); err != nil {
			t.Fatalf("Identify: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("WhoIs called %d times inside the TTL, want 1", calls.Load())
		}

		synctest.Sleep(31 * time.Second) // now past the minute
		if _, err := r.Identify(ctx, "100.64.0.1:1"); err != nil {
			t.Fatalf("Identify: %v", err)
		}
		if calls.Load() != 2 {
			t.Errorf("WhoIs called %d times after the TTL, want 2", calls.Load())
		}
	})
}

func TestFlushClearsTheCache(t *testing.T) {
	var calls atomic.Int64
	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		calls.Add(1)
		return alice, nil
	})
	ctx := context.Background()

	r.Identify(ctx, "100.64.0.1:1")
	r.Flush()
	r.Identify(ctx, "100.64.0.1:1")

	if calls.Load() != 2 {
		t.Errorf("WhoIs called %d times, want 2 after a flush", calls.Load())
	}
}

func TestLookupByLogin(t *testing.T) {
	r, _ := newResolver(t, staticWhoIs(alice))
	ctx := context.Background()

	created, err := r.Identify(ctx, "100.64.0.1:1")
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}

	found, err := r.ByLogin(ctx, "alice@github")
	if err != nil {
		t.Fatalf("ByLogin: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("ByLogin returned %+v, want the same user", found)
	}

	if _, err := r.ByLogin(ctx, "nobody@github"); !errors.Is(err, identity.ErrUnknownUser) {
		t.Errorf("ByLogin(unknown) = %v, want ErrUnknownUser", err)
	}
}

func TestConcurrentFirstSight(t *testing.T) {
	// Several requests from a brand new user can land at once. Exactly one user
	// row must result.
	r, db := newResolver(t, staticWhoIs(alice))
	ctx := context.Background()

	errs := make(chan error, 16)
	for i := range 16 {
		go func() {
			// Distinct source addresses so nothing is served from cache.
			_, err := r.Identify(ctx, fmt.Sprintf("100.64.0.1:%d", 1000+i))
			errs <- err
		}()
	}
	for range 16 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Identify: %v", err)
		}
	}

	n, _ := db.One[int](ctx, `SELECT count(*) FROM users`)
	if n != 1 {
		t.Errorf("users rows = %d, want exactly 1", n)
	}
}

func TestOnNewUserFiresOnceForABrandNewUser(t *testing.T) {
	// A vault comes into existence on someone's first request, not at startup.
	// Anything that works per vault, the file watcher above all, has to be told.
	var seen []identity.User
	r, _ := newResolver(t, staticWhoIs(alice), func(o *identity.Options) {
		o.OnNewUser = func(_ context.Context, u identity.User) { seen = append(seen, u) }
	})
	ctx := context.Background()

	if _, err := r.Identify(ctx, "100.64.0.1:1"); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("hook fired %d times on a first sighting, want 1", len(seen))
	}
	if seen[0].VaultDir != "alice-github" || seen[0].VaultID == 0 {
		t.Errorf("hook received %+v, want a fully provisioned user", seen[0])
	}

	// A second request, from a different device so the cache is not involved,
	// must not fire it again.
	r.Flush()
	if _, err := r.Identify(ctx, "100.64.0.2:1"); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(seen) != 1 {
		t.Errorf("hook fired %d times, want 1; it must only report genuinely new users", len(seen))
	}
}

func TestOnNewUserDoesNotFireForAnAgent(t *testing.T) {
	// An agent borrows an existing identity, so there is no new vault.
	var fired int
	tagged := identity.WhoIs{Tags: []string{"tag:notes-agent"}, NodeName: "agent"}
	var who atomic.Pointer[identity.WhoIs]
	who.Store(&alice)

	r, _ := newResolver(t, func(context.Context, string) (identity.WhoIs, error) {
		return *who.Load(), nil
	}, func(o *identity.Options) {
		o.Agents = map[string]string{"tag:notes-agent": "alice@github"}
		o.OnNewUser = func(context.Context, identity.User) { fired++ }
	})
	ctx := context.Background()

	r.Identify(ctx, "100.64.0.1:1")
	who.Store(&tagged)
	r.Flush()
	r.Identify(ctx, "100.64.0.9:1")

	if fired != 1 {
		t.Errorf("hook fired %d times, want 1 (for alice, not for her agent)", fired)
	}
}
