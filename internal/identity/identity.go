// Package identity turns a tailnet connection into a tsnotes user.
//
// There are no passwords, sessions, or cookies anywhere in tsnotes. The tailnet
// already knows who is on the other end of the connection, so every request asks
// tailscaled's WhoIs for the peer behind the source address and works from that.
// This is the whole authentication story, which is why the failure modes here
// are worth being careful about: a WhoIs that fails is "try again shortly", not
// "you are not allowed".
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/scottjab/tsnotes/internal/store"
	"github.com/scottjab/tsnotes/internal/vaultpath"
)

var (
	// ErrNoIdentity means the peer is real but has no human behind it, such as
	// a tagged node with no configured mapping. This is a 403.
	ErrNoIdentity = errors.New("no user identity for this connection")
	// ErrUnavailable means we could not ask. This is a 503, never a 403.
	ErrUnavailable = errors.New("identity service unavailable")
	// ErrUnknownUser means a lookup by login found nobody.
	ErrUnknownUser = errors.New("unknown user")
)

// WhoIs is the subset of tailscale's WhoIsResponse that tsnotes cares about.
//
// Declaring our own keeps the tailscale dependency confined to internal/tsserve
// and makes every test in this package run without a tailnet.
type WhoIs struct {
	// UserID is the stable tailscale user id. It, not the login, is the
	// identity: logins change.
	UserID      int64
	Login       string
	DisplayName string
	ProfilePic  string
	// Tags is non-empty for a tagged node, which has no user behind it.
	Tags     []string
	NodeName string
}

// IsTagged reports whether the peer is a tagged node rather than a person.
func (w WhoIs) IsTagged() bool { return len(w.Tags) > 0 }

// WhoIsFunc resolves a remote address to a tailnet peer.
type WhoIsFunc func(ctx context.Context, remoteAddr string) (WhoIs, error)

// User is an authenticated tsnotes user and the vault they own.
type User struct {
	ID          int64
	TailscaleID int64
	Login       string
	DisplayName string
	ProfilePic  string
	VaultID     int64
	VaultDir    string

	// IsAgent is true when the request came from a tagged node acting as this
	// user. Handlers can use it for audit logging; permissions are identical.
	IsAgent  bool
	AgentTag string
}

// Options configures a Resolver.
type Options struct {
	// CacheTTL bounds how long a WhoIs answer is reused. Short enough that
	// revoking a device takes effect promptly, long enough that we are not
	// hitting the local API on every request for a page's worth of assets.
	CacheTTL time.Duration

	// Agents maps a node tag to the login it may act as, which is how an AI
	// agent on a tagged server gets access to exactly one person's notes.
	Agents map[string]string

	// OnNewUser is called once, just after a user and their vault are created.
	//
	// A vault comes into existence on someone's first request, not at startup,
	// so anything that operates per vault (the file watcher, most obviously)
	// needs telling. Without this, a brand new user's vault would go unwatched
	// until the next restart.
	OnNewUser func(context.Context, User)
}

// Resolver maps connections to users, creating a user and vault on first sight.
type Resolver struct {
	db    *store.DB
	whois WhoIsFunc
	opts  Options

	mu    sync.Mutex
	cache map[netip.Addr]cacheEntry
	// create serializes first-sight user creation so a burst of requests from a
	// new user cannot race into duplicate rows.
	create sync.Mutex
}

type cacheEntry struct {
	user    User
	expires time.Time
}

// NewResolver returns a Resolver. A zero CacheTTL disables caching.
func NewResolver(db *store.DB, whois WhoIsFunc, opts Options) *Resolver {
	return &Resolver{
		db:    db,
		whois: whois,
		opts:  opts,
		cache: map[netip.Addr]cacheEntry{},
	}
}

// Identify resolves an http.Request's RemoteAddr to a user, provisioning the
// user and their vault the first time we see them.
func (r *Resolver) Identify(ctx context.Context, remoteAddr string) (User, error) {
	addr, err := parseAddr(remoteAddr)
	if err != nil {
		return User{}, err
	}

	if u, ok := r.cached(addr); ok {
		return u, nil
	}

	who, err := r.whois(ctx, remoteAddr)
	if err != nil {
		return User{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	login, agentTag := who.Login, ""
	if who.IsTagged() {
		login, agentTag = r.agentLogin(who)
		if login == "" {
			return User{}, fmt.Errorf("%w: node %q is tagged %v with no configured mapping",
				ErrNoIdentity, who.NodeName, who.Tags)
		}
	}
	if login == "" {
		return User{}, fmt.Errorf("%w: peer has no login", ErrNoIdentity)
	}

	u, fresh, err := r.provision(ctx, who, login, agentTag)
	if err != nil {
		return User{}, err
	}
	if fresh && r.opts.OnNewUser != nil {
		r.opts.OnNewUser(ctx, u)
	}
	r.store(addr, u)
	return u, nil
}

// agentLogin finds the login a tagged node is configured to act as.
func (r *Resolver) agentLogin(who WhoIs) (login, tag string) {
	for _, t := range who.Tags {
		if l, ok := r.opts.Agents[t]; ok {
			return l, t
		}
	}
	return "", ""
}

// parseAddr extracts the peer IP. The cache is keyed on the address alone
// because every new HTTP connection from the same device brings a new source
// port, and keying on the pair would mean never hitting the cache.
func parseAddr(remoteAddr string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr(), nil
	}
	addr, err := netip.ParseAddr(remoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: cannot parse remote address %q", ErrNoIdentity, remoteAddr)
	}
	return addr, nil
}

func (r *Resolver) cached(addr netip.Addr) (User, bool) {
	if r.opts.CacheTTL <= 0 {
		return User{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[addr]
	if !ok || time.Now().After(e.expires) {
		return User{}, false
	}
	return e.user, true
}

func (r *Resolver) store(addr netip.Addr, u User) {
	if r.opts.CacheTTL <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[addr] = cacheEntry{user: u, expires: time.Now().Add(r.opts.CacheTTL)}
}

// Flush empties the cache. Called when a user's record changes, and by tests.
func (r *Resolver) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.cache)
}

// userRow mirrors the users table.
type userRow struct {
	ID              int64  `db:"id"`
	TailscaleUserID int64  `db:"tailscale_user_id"`
	Login           string `db:"login"`
	DisplayName     string `db:"display_name"`
	ProfilePicURL   string `db:"profile_pic_url"`
	VaultDir        string `db:"vault_dir"`
}

// provision finds or creates the user row and their vault row, reporting
// whether this was the first time we had seen them.
func (r *Resolver) provision(ctx context.Context, who WhoIs, login, agentTag string) (User, bool, error) {
	// An agent acting as someone must not be able to conjure that someone into
	// existence; it can only borrow an identity that already exists.
	if agentTag != "" {
		u, err := r.ByLogin(ctx, login)
		if err != nil {
			return User{}, false, fmt.Errorf("%w: tag %s maps to %q, which is not a known user: %w",
				ErrNoIdentity, agentTag, login, err)
		}
		u.IsAgent, u.AgentTag = true, agentTag
		return u, false, nil
	}

	r.create.Lock()
	defer r.create.Unlock()

	fresh := false
	row, err := r.db.One[userRow](ctx,
		`SELECT id, tailscale_user_id, login, display_name, profile_pic_url, vault_dir
		 FROM users WHERE tailscale_user_id = ?`, who.UserID)
	switch {
	case err == nil:
		if row.Login != login || row.DisplayName != who.DisplayName || row.ProfilePicURL != who.ProfilePic {
			// The login is only a label. vault_dir stays as it is, which is what
			// keeps a rename from orphaning the notes.
			if _, err := r.db.Exec(ctx,
				`UPDATE users SET login = ?, display_name = ?, profile_pic_url = ?, last_seen_at = ? WHERE id = ?`,
				login, who.DisplayName, who.ProfilePic, time.Now().Unix(), row.ID); err != nil {
				return User{}, false, fmt.Errorf("update user %d: %w", row.ID, err)
			}
			row.Login, row.DisplayName, row.ProfilePicURL = login, who.DisplayName, who.ProfilePic
		}
	case errors.Is(err, sql.ErrNoRows):
		row, err = r.insertUser(ctx, who, login)
		if err != nil {
			return User{}, false, err
		}
		fresh = true
	default:
		return User{}, false, fmt.Errorf("look up user: %w", err)
	}

	vaultID, err := r.ensureVault(ctx, row.ID, row.VaultDir)
	if err != nil {
		return User{}, false, err
	}
	return User{
		ID: row.ID, TailscaleID: row.TailscaleUserID, Login: row.Login,
		DisplayName: row.DisplayName, ProfilePic: row.ProfilePicURL,
		VaultID: vaultID, VaultDir: row.VaultDir,
	}, fresh, nil
}

func (r *Resolver) insertUser(ctx context.Context, who WhoIs, login string) (userRow, error) {
	dir, err := r.freeVaultDir(ctx, login)
	if err != nil {
		return userRow{}, err
	}
	now := time.Now().Unix()
	if _, err := r.db.Exec(ctx,
		`INSERT INTO users (tailscale_user_id, login, display_name, profile_pic_url, vault_dir, created_at, last_seen_at)
		 VALUES (?,?,?,?,?,?,?)`,
		who.UserID, login, who.DisplayName, who.ProfilePic, dir, now, now); err != nil {
		return userRow{}, fmt.Errorf("create user %q: %w", login, err)
	}
	return r.db.One[userRow](ctx,
		`SELECT id, tailscale_user_id, login, display_name, profile_pic_url, vault_dir
		 FROM users WHERE tailscale_user_id = ?`, who.UserID)
}

// freeVaultDir slugs the login and, if that directory is taken by a different
// user, appends a counter. Two logins can slug to the same thing, and sharing a
// directory would mean sharing notes.
func (r *Resolver) freeVaultDir(ctx context.Context, login string) (string, error) {
	base := vaultpath.Slug(login)
	for n := range 1000 {
		candidate := base
		if n > 0 {
			candidate = fmt.Sprintf("%s-%d", base, n+1)
		}
		_, err := r.db.One[int64](ctx, `SELECT id FROM users WHERE vault_dir = ?`, candidate)
		if errors.Is(err, sql.ErrNoRows) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("check vault dir %q: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("no free vault directory for %q", login)
}

// ensureVault finds or creates the vaults row for a user.
func (r *Resolver) ensureVault(ctx context.Context, userID int64, dir string) (int64, error) {
	id, err := r.db.One[int64](ctx, `SELECT id FROM vaults WHERE user_id = ?`, userID)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("look up vault: %w", err)
	}
	if _, err := r.db.Exec(ctx,
		`INSERT INTO vaults (user_id, dir, created_at) VALUES (?,?,?)`,
		userID, dir, time.Now().Unix()); err != nil {
		return 0, fmt.Errorf("create vault for user %d: %w", userID, err)
	}
	return r.db.One[int64](ctx, `SELECT id FROM vaults WHERE user_id = ?`, userID)
}

// ByLogin looks up an existing user. It never creates one: sharing a note with
// someone who has not logged in yet has to fail loudly, or you would think you
// had shared it when you had not.
func (r *Resolver) ByLogin(ctx context.Context, login string) (User, error) {
	row, err := r.db.One[userRow](ctx,
		`SELECT id, tailscale_user_id, login, display_name, profile_pic_url, vault_dir
		 FROM users WHERE login = ? COLLATE NOCASE`, login)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("%w: %s", ErrUnknownUser, login)
	}
	if err != nil {
		return User{}, fmt.Errorf("look up %q: %w", login, err)
	}
	vaultID, err := r.ensureVault(ctx, row.ID, row.VaultDir)
	if err != nil {
		return User{}, err
	}
	return User{
		ID: row.ID, TailscaleID: row.TailscaleUserID, Login: row.Login,
		DisplayName: row.DisplayName, ProfilePic: row.ProfilePicURL,
		VaultID: vaultID, VaultDir: row.VaultDir,
	}, nil
}

// ByVaultID resolves a vault back to its owner, which the API needs when
// rendering "shared with me" listings.
func (r *Resolver) ByVaultID(ctx context.Context, vaultID int64) (User, error) {
	row, err := r.db.One[userRow](ctx, `
		SELECT u.id, u.tailscale_user_id, u.login, u.display_name, u.profile_pic_url, u.vault_dir
		FROM users u JOIN vaults v ON v.user_id = u.id WHERE v.id = ?`, vaultID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("%w: vault %d", ErrUnknownUser, vaultID)
	}
	if err != nil {
		return User{}, fmt.Errorf("look up vault %d owner: %w", vaultID, err)
	}
	return User{
		ID: row.ID, TailscaleID: row.TailscaleUserID, Login: row.Login,
		DisplayName: row.DisplayName, ProfilePic: row.ProfilePicURL,
		VaultID: vaultID, VaultDir: row.VaultDir,
	}, nil
}

// Users lists everyone tsnotes has seen, which is what the share dialog offers
// as autocomplete.
func (r *Resolver) Users(ctx context.Context) ([]User, error) {
	rows, err := r.db.All[userRow](ctx,
		`SELECT id, tailscale_user_id, login, display_name, profile_pic_url, vault_dir FROM users ORDER BY login`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	out := make([]User, len(rows))
	for i, row := range rows {
		out[i] = User{
			ID: row.ID, TailscaleID: row.TailscaleUserID, Login: row.Login,
			DisplayName: row.DisplayName, ProfilePic: row.ProfilePicURL, VaultDir: row.VaultDir,
		}
	}
	return out, nil
}
