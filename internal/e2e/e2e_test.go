package e2e

// The tests themselves. Every test builds a fresh tailnet: new control server,
// new DERP, new nodes. That costs a few seconds each and buys tests that do
// not order-depend on each other.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/events"
)

func init() {
	flag.BoolVar(&verbose, "verbose-tsnet", false,
		"pass tsnet and DERP logs through to the test output")
}

// eventually runs op until it succeeds or the budget runs out, and fails the
// test with the last error.
//
// The first request a freshly joined node makes races the control server's map
// push: the node is Running before its peer list or its WireGuard session are
// fully warm. Production clients retry (the TUI reconnects, browsers
// re-request); a test that wants to be about something other than startup
// timing retries too.
func eventually(t *testing.T, what string, op func() error) {
	t.Helper()
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; ; i++ {
		if err = op(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		sleep := time.Duration(i+1) * 250 * time.Millisecond
		if sleep > 2*time.Second {
			sleep = 2 * time.Second
		}
		time.Sleep(sleep)
	}
	t.Fatalf("%s: still failing after 30s: %v", what, err)
}

// statusOf unwraps a client error's status. Zero when err is not a client
// error, which the assertions treat as "wrong kind of failure" and let the
// test fail loudly.
func statusOf(err error) int {
	var ce *client.Error
	if errors.As(err, &ce) {
		return ce.Status
	}
	return 0
}

// TestIdentityEndToEnd is the test this package exists for: a real node joins
// a real control server, makes a real request across the tailnet, and folio
// knows exactly who is on the other end, provisions their vault, and puts
// their note on disk.
func TestIdentityEndToEnd(t *testing.T) {
	ctx := context.Background()
	ctrl := newControl(t)
	alice := ctrl.user("alice")
	folio := startFolio(ctx, t, ctrl, ctrl.user("folio").key(), folioOptions{})
	laptop := joinNode(t, ctrl, "alice-laptop", alice.ephemeralKey())
	cl := laptop.client(folio)

	// The tailnet says who we are.
	var me client.Me
	eventually(t, "first request from a fresh node", func() error {
		var err error
		me, err = cl.Me(ctx)
		return err
	})
	if me.Login != "alice" {
		t.Errorf("Me().Login = %q, want %q (WhoIs resolved the wrong peer)", me.Login, "alice")
	}
	if me.IsAgent {
		t.Error("Me().IsAgent = true, want false for a personal node")
	}
	if me.Vault == "" {
		t.Fatal("Me().Vault is empty: no vault was provisioned")
	}

	// A note written over the tailnet lands on the server's disk, in the
	// vault the identity resolved to.
	sum, err := cl.CreateNote(ctx, me.Vault, "Welcome.md", "# Welcome\n\nHello from alice-laptop.")
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if sum.SHA256 == "" {
		t.Error("CreateNote returned an empty SHA256")
	}

	var note client.Note
	eventually(t, "Note after create", func() error {
		note, err = cl.Note(ctx, me.Vault, "Welcome.md")
		return err
	})
	if !strings.Contains(note.Content, "Hello from alice-laptop.") {
		t.Errorf("note content %q lost what was written over the tailnet", note.Content)
	}
	if !strings.Contains(note.Content, "id: ") {
		t.Errorf("note content %q has no id frontmatter: identity was not stamped", note.Content)
	}
	if note.OwnerLogin != "alice" {
		t.Errorf("note.OwnerLogin = %q, want %q", note.OwnerLogin, "alice")
	}

	// The file is really on disk, in alice's vault, under folio's state dir.
	onDisk, err := os.ReadFile(filepath.Join(folio.vaultsDir, me.Vault, "Welcome.md"))
	if err != nil {
		t.Fatalf("read the note from disk: %v", err)
	}
	if !strings.Contains(string(onDisk), "Hello from alice-laptop.") {
		t.Errorf("file on disk = %q, does not match what was written", onDisk)
	}

	// The index saw it too.
	res, err := cl.Search(ctx, "alice-laptop", 10, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range res.Hits {
		if h.Path == "Welcome.md" && h.OwnerLogin == "alice" {
			found = true
		}
	}
	if !found {
		t.Errorf("search over the tailnet found nothing for the note created over the tailnet: %+v", res.Hits)
	}

	// And a compare-and-swap write round-trips the content hash the server
	// handed out.
	if _, err := cl.UpdateNote(ctx, me.Vault, "Welcome.md",
		"# Welcome\n\nEdited over the tailnet.", sum.SHA256); err != nil {
		t.Fatalf("UpdateNote with the server's own hash: %v", err)
	}
	_, err = cl.UpdateNote(ctx, me.Vault, "Welcome.md", "# Stale", "not-the-real-hash")
	if got := statusOf(err); got != http.StatusConflict {
		t.Errorf("stale-baseSHA write: status = %d, want 409 (err = %v)", got, err)
	}
}

// TestShareEndToEnd walks a grant across the tailnet: alice shares a note with
// bob, bob reads it from alice's vault, the permission boundary holds against
// bob's pen and against carol entirely, and revoking closes the door.
func TestShareEndToEnd(t *testing.T) {
	ctx := context.Background()
	ctrl := newControl(t)
	alice := ctrl.user("alice")
	bob := ctrl.user("bob")
	carol := ctrl.user("carol")
	folio := startFolio(ctx, t, ctrl, ctrl.user("folio").key(), folioOptions{})

	aLaptop := joinNode(t, ctrl, "alice-laptop", alice.ephemeralKey())
	bLaptop := joinNode(t, ctrl, "bob-laptop", bob.ephemeralKey())
	cTablet := joinNode(t, ctrl, "carol-tablet", carol.ephemeralKey())
	aCl, bCl, cCl := aLaptop.client(folio), bLaptop.client(folio), cTablet.client(folio)

	me, err := aCl.Me(ctx)
	if err != nil {
		t.Fatalf("alice Me: %v", err)
	}
	if _, err := aCl.CreateNote(ctx, me.Vault, "Project/Notes.md", "# Project\n\nShared soon."); err != nil {
		t.Fatalf("alice CreateNote: %v", err)
	}

	// Bob discovers alice's vault the way the UI does: it appears in the
	// vault list once shared, not before. The first call also warms bob's
	// connection to folio, so later assertions are not racing discovery.
	var vaults []client.Vault
	eventually(t, "bob's first request", func() error {
		var err error
		vaults, err = bCl.Vaults(ctx)
		return err
	})
	for _, v := range vaults {
		if v.OwnerLogin == "alice" {
			t.Fatalf("bob sees alice's vault %q before alice shared anything", v.Vault)
		}
	}

	if _, err := aCl.Grant(ctx, "Project/Notes.md", false, "bob", client.PermRead); err != nil {
		t.Fatalf("alice Grant read to bob: %v", err)
	}

	// Bob can now see the vault and read the note.
	vaults, err = bCl.Vaults(ctx)
	if err != nil {
		t.Fatalf("bob Vaults: %v", err)
	}
	aliceVault := ""
	for _, v := range vaults {
		if v.OwnerLogin == "alice" {
			aliceVault = v.Vault
		}
	}
	if aliceVault == "" {
		t.Fatal("bob still cannot see alice's vault after the grant")
	}
	bn, err := bCl.Note(ctx, aliceVault, "Project/Notes.md")
	if err != nil {
		t.Fatalf("bob read of alice's shared note: %v", err)
	}
	if bn.Perm != client.PermRead {
		t.Errorf("bob's perm on the shared note = %q, want read", bn.Perm)
	}

	// A read grant does not become a write grant on the far side of the wire.
	_, err = bCl.UpdateNote(ctx, aliceVault, "Project/Notes.md", "# Overwritten", bn.SHA256)
	if got := statusOf(err); got != http.StatusForbidden {
		t.Errorf("bob write with read grant: status = %d, want 403 (err = %v)", got, err)
	}

	// Carol, on the same tailnet, sees nothing. Her first request warms her
	// connection, so the refusal below is a real answer, not a startup race.
	var cVaults []client.Vault
	eventually(t, "carol's first request", func() error {
		var err error
		cVaults, err = cCl.Vaults(ctx)
		return err
	})
	_, err = cCl.Note(ctx, aliceVault, "Project/Notes.md")
	if got := statusOf(err); got != http.StatusForbidden {
		t.Errorf("carol read of an unshared note: status = %d, want 403 (err = %v)", got, err)
	}
	for _, v := range cVaults {
		if v.OwnerLogin == "alice" {
			t.Errorf("carol sees alice's vault %q with no grant", v.Vault)
		}
	}

	// Widening the grant lets bob write, and alice sees bob's words.
	if _, err := aCl.Grant(ctx, "Project/Notes.md", false, "bob", client.PermWrite); err != nil {
		t.Fatalf("alice Grant write to bob: %v", err)
	}
	updated, err := bCl.UpdateNote(ctx, aliceVault, "Project/Notes.md",
		"# Project\n\nBob was here.", bn.SHA256)
	if err != nil {
		t.Fatalf("bob write with write grant: %v", err)
	}
	an, err := aCl.Note(ctx, me.Vault, "Project/Notes.md")
	if err != nil {
		t.Fatalf("alice read-back: %v", err)
	}
	if an.SHA256 == bn.SHA256 || an.SHA256 != updated.SHA256 {
		t.Error("alice's copy did not converge on bob's write")
	}

	// Revoking closes the door again.
	shares, err := aCl.Shares(ctx)
	if err != nil {
		t.Fatalf("alice Shares: %v", err)
	}
	if len(shares) == 0 {
		t.Fatal("alice has no shares to revoke")
	}
	if err := aCl.Revoke(ctx, shares[0].ID); err != nil {
		t.Fatalf("alice Revoke: %v", err)
	}
	eventually(t, "revocation to take effect for bob", func() error {
		_, err := bCl.Note(ctx, aliceVault, "Project/Notes.md")
		if err == nil {
			return fmt.Errorf("bob can still read the revoked note")
		}
		if got := statusOf(err); got != http.StatusForbidden && got != http.StatusNotFound {
			return fmt.Errorf("bob read after revoke: status = %d, want 403 or 404", got)
		}
		return nil
	})
}

// TestTaggedNodeAsAgent checks the agent path over the tailnet: a headless,
// tagged node has no person behind it, so it is refused until the operator
// maps its tag to a user, and then it acts as that user and nobody else.
func TestTaggedNodeAsAgent(t *testing.T) {
	ctx := context.Background()
	ctrl := newControl(t)
	alice := ctrl.user("alice")
	folio := startFolio(ctx, t, ctrl, ctrl.user("folio").key(), folioOptions{
		Agents: map[string]string{"tag:agent": "alice"},
	})

	// Alice goes first: an agent may borrow an identity that exists, never
	// conjure one. This is the first request, so it also settles alice's
	// provisioning before the agent can be judged against it.
	aLaptop := joinNode(t, ctrl, "alice-laptop", alice.ephemeralKey())
	aCl := aLaptop.client(folio)
	var me client.Me
	eventually(t, "alice's first request", func() error {
		var err error
		me, err = aCl.Me(ctx)
		return err
	})

	// The tagged node, mapped, is alice with a flag saying so.
	agent := joinNode(t, ctrl, "research-agent", alice.taggedKey("tag:agent"))
	agCl := agent.client(folio)
	var ame client.Me
	eventually(t, "agent's first request", func() error {
		var err error
		ame, err = agCl.Me(ctx)
		return err
	})
	if !ame.IsAgent || ame.Login != "alice" {
		t.Errorf("agent Me() = {login %q, isAgent %v}, want alice as agent", ame.Login, ame.IsAgent)
	}
	if _, err := agCl.CreateNote(ctx, me.Vault, "From-agent.md", "# From the agent"); err != nil {
		t.Fatalf("agent CreateNote into alice's vault: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(folio.vaultsDir, me.Vault, "From-agent.md"))
	if err != nil {
		t.Fatalf("agent's note did not reach alice's vault on disk: %v", err)
	}
	if !strings.Contains(string(onDisk), "From the agent") {
		t.Errorf("agent's note on disk = %q", onDisk)
	}

	// A tagged node whose tag nobody mapped is a machine with no person
	// behind it: refused, and 403, not 503.
	other := ctrl.user("ops")
	stray := joinNode(t, ctrl, "unmapped-agent", other.taggedKey("tag:unmapped"))
	strayCl := stray.client(folio)
	eventually(t, "stray node's first request", func() error {
		_, err := strayCl.Me(ctx)
		if err == nil {
			return fmt.Errorf("unmapped tagged node was let in")
		}
		if got := statusOf(err); got != http.StatusForbidden {
			return fmt.Errorf("unmapped tagged node: status = %d, want 403", got)
		}
		return nil
	})
}

// TestCSRFBlockedOverTailnet proves the cross-origin guard survives a real
// tailnet hop: folio authenticated by network position must still refuse a
// state-changing request that a browser was tricked into sending.
func TestCSRFBlockedOverTailnet(t *testing.T) {
	ctx := context.Background()
	ctrl := newControl(t)
	alice := ctrl.user("alice")
	folio := startFolio(ctx, t, ctrl, ctrl.user("folio").key(), folioOptions{})
	laptop := joinNode(t, ctrl, "alice-laptop", alice.ephemeralKey())
	cl := laptop.client(folio)

	var me client.Me
	eventually(t, "alice's first request", func() error {
		var err error
		me, err = cl.Me(ctx)
		return err
	})

	post := func(origin, path string) int {
		t.Helper()
		body := strings.NewReader(fmt.Sprintf(`{"path":%q,"content":"# csrf"}`, path))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			folio.url+"/api/vaults/"+me.Vault+"/notes", body)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := laptop.ts.HTTPClient().Do(req)
		if err != nil {
			t.Fatalf("POST with Origin %q: %v", origin, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("https://evil.example.com", "Blocked.md"); got != http.StatusForbidden {
		t.Errorf("cross-origin POST: status = %d, want 403", got)
	}
	// The same write from folio's own origin, or with no Origin at all (a
	// terminal client), goes through.
	if got := post(folio.url, "OwnOrigin.md"); got != http.StatusCreated {
		t.Errorf("same-origin POST: status = %d, want 201", got)
	}
	if got := post("", "NoOrigin.md"); got != http.StatusCreated {
		t.Errorf("origin-less POST: status = %d, want 201", got)
	}
}

// TestMagicDNSAndIPDial checks both ways a client finds folio on the tailnet:
// the MagicDNS name (what `folio tui` builds from the node named "folio") and
// the bare tailnet IP.
func TestMagicDNSAndIPDial(t *testing.T) {
	ctx := context.Background()
	ctrl := newControl(t)
	alice := ctrl.user("alice")
	folio := startFolio(ctx, t, ctrl, ctrl.user("folio").key(), folioOptions{})
	laptop := joinNode(t, ctrl, "alice-laptop", alice.ephemeralKey())

	// The tailnet advertises the MagicDNS suffix, which is how the TUI turns
	// "the node called folio" into an address without being told.
	lc, err := laptop.ts.LocalClient()
	if err != nil {
		t.Fatalf("client node local API: %v", err)
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		t.Fatalf("client node status: %v", err)
	}
	if st.CurrentTailnet == nil {
		t.Fatal("client node reports no current tailnet")
	}
	if got := strings.TrimSuffix(st.CurrentTailnet.MagicDNSSuffix, "."); got != magicDNSDomain {
		t.Errorf("MagicDNSSuffix = %q, want %q", got, magicDNSDomain)
	}

	// By name: this is the URL every other test uses too, so resolution must
	// already work; assert it explicitly with a clean client.
	byName, err := client.New(folio.url, client.WithHTTPClient(laptop.ts.HTTPClient()))
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	var me client.Me
	eventually(t, "Me by MagicDNS name", func() error {
		var err error
		me, err = byName.Me(ctx)
		return err
	})
	if me.Login != "alice" {
		t.Errorf("Me() by MagicDNS name: login = %q, want alice", me.Login)
	}

	// By IP: same answer, no DNS involved.
	byIP, err := client.New(fmt.Sprintf("http://%s:%d", folio.ip, folioPort),
		client.WithHTTPClient(laptop.ts.HTTPClient()))
	if err != nil {
		t.Fatalf("build client by IP: %v", err)
	}
	ipMe, err := byIP.Me(ctx)
	if err != nil {
		t.Fatalf("Me by tailnet IP: %v", err)
	}
	if ipMe.Login != "alice" {
		t.Errorf("Me() by IP: login = %q, want alice", ipMe.Login)
	}
}

// TestEventsAcrossNodes checks the live-update path over the tailnet: alice
// sits on the SSE stream, bob (via a write grant) edits the shared note, and
// the change reaches alice's client while she watches.
func TestEventsAcrossNodes(t *testing.T) {
	ctx := context.Background()
	ctrl := newControl(t)
	alice := ctrl.user("alice")
	bob := ctrl.user("bob")
	folio := startFolio(ctx, t, ctrl, ctrl.user("folio").key(), folioOptions{})

	aLaptop := joinNode(t, ctrl, "alice-laptop", alice.ephemeralKey())
	bLaptop := joinNode(t, ctrl, "bob-laptop", bob.ephemeralKey())
	aCl, bCl := aLaptop.client(folio), bLaptop.client(folio)

	me, err := aCl.Me(ctx)
	if err != nil {
		t.Fatalf("alice Me: %v", err)
	}
	// Sharing with bob needs bob to exist in folio's book of users, and that
	// book only records people who have connected at least once. Bob's first
	// request is the provision; it also warms his node.
	eventually(t, "bob's first request", func() error {
		_, err := bCl.Me(ctx)
		return err
	})
	if _, err := aCl.CreateNote(ctx, me.Vault, "Live.md", "# Live"); err != nil {
		t.Fatalf("alice CreateNote: %v", err)
	}
	if _, err := aCl.Grant(ctx, "Live.md", false, "bob", client.PermWrite); err != nil {
		t.Fatalf("alice Grant: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := aCl.Watch(watchCtx)

	// The stream announces itself. Wait for that, or bob's write could land
	// before alice is listening and the test would flake on timing it cannot
	// see.
	deadline := time.After(30 * time.Second)
	for connected := false; !connected; {
		select {
		case ev, ok := <-stream:
			if !ok {
				t.Fatal("event stream closed before connecting")
			}
			if ev.Err != nil {
				t.Errorf("stream error during connect: %v", ev.Err)
				continue
			}
			connected = ev.Connected && ev.Change == nil
		case <-deadline:
			t.Fatal("event stream never connected")
		}
	}

	// Bob writes from his own node, over his own WireGuard session.
	bn, err := bCl.Note(ctx, me.Vault, "Live.md")
	if err != nil {
		t.Fatalf("bob read of shared note: %v", err)
	}
	if _, err := bCl.UpdateNote(ctx, me.Vault, "Live.md", "# Live\n\nBob.", bn.SHA256); err != nil {
		t.Fatalf("bob UpdateNote: %v", err)
	}

	// Alice's stream carries the change: not from polling, from the event bus.
	deadline = time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				t.Fatal("event stream closed before the change arrived")
			}
			if ev.Err != nil {
				t.Errorf("stream error: %v", ev.Err)
				continue
			}
			if ev.Change != nil && ev.Change.Path == "Live.md" && ev.Change.Vault == me.Vault {
				if ev.Change.Kind != events.NoteUpdated {
					t.Errorf("event kind = %q, want %q", ev.Change.Kind, events.NoteUpdated)
				}
				return // the change crossed the tailnet
			}
		case <-deadline:
			t.Fatal("alice's stream never saw bob's edit")
		}
	}
}
