// Package config loads and validates tsnotes' settings.
//
// Settings come from three places, in increasing precedence: the defaults here,
// an optional JSON file, and then flags and environment variables. The one thing
// that is not in the file is the tailnet auth key, which is a credential and
// belongs in the environment (or a systemd LoadCredential) rather than in a file
// people copy between machines.
package config

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Agent maps a tailnet node tag to the user an agent on that node may act as.
type Agent struct {
	Tag   string `json:"tag"`
	ActAs string `json:"actAs"`
}

// Config is the whole of tsnotes' configuration.
type Config struct {
	// Hostname is the tsnet node name, so the app lands at
	// https://<hostname>.<tailnet>.ts.net.
	Hostname string `json:"hostname"`
	// StateDir holds the database, the tsnet node state, and every vault.
	StateDir string `json:"stateDir"`
	// Addr is the address tsnet listens on inside the tailnet.
	Addr string `json:"addr"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `json:"logLevel"`
	// CacheTTL bounds how long a WhoIs answer is reused.
	CacheTTL Duration `json:"cacheTTL"`
	// WatchExternal enables the filesystem watcher that notices edits made in
	// Obsidian or by a git pull.
	WatchExternal bool `json:"watchExternal"`
	// Agents lets a tagged node act as a named user.
	Agents []Agent `json:"agents"`
	// DevAddr, when set, also serves plain HTTP on a local address. It bypasses
	// the tailnet entirely and is for development only.
	DevAddr string `json:"devAddr"`
	// DevLogin is the identity to assume on the DevAddr listener, since there is
	// no tailnet peer to ask about.
	DevLogin string `json:"devLogin"`

	// AuthKey is never read from the config file; see ApplyEnv.
	AuthKey string `json:"-"`
}

// Duration is a time.Duration that reads as a string in JSON, so a config can
// say "45s" instead of 45000000000.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalJSON writes the duration in its human form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts either "45s" or a plain number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"45s\" or a number of nanoseconds")
	}
	*d = Duration(n)
	return nil
}

// Default returns the configuration tsnotes runs with when told nothing.
func Default() Config {
	return Config{
		Hostname:      "tsnotes",
		StateDir:      defaultStateDir(),
		Addr:          ":443",
		LogLevel:      "info",
		CacheTTL:      Duration(30 * time.Second),
		WatchExternal: true,
	}
}

// defaultStateDir follows the XDG state convention, falling back to the working
// directory if there is no home to speak of.
func defaultStateDir() string {
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".local", "state", "tsnotes")
	}
	return "tsnotes-state"
}

// Load reads a config file over the defaults. A missing file is not an error:
// running with no config at all is the common case.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}

	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read config %s: %w", path, err)
	}

	// RejectUnknownMembers is the point of using json/v2 here. A typo'd key that
	// is silently ignored looks exactly like a setting that did not work, and
	// costs an hour to find.
	if err := json.Unmarshal(body, &c, json.RejectUnknownMembers(true)); err != nil {
		return c, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// ApplyEnv layers environment variables over the config. TS_AUTHKEY is the only
// way to supply an auth key.
func (c *Config) ApplyEnv() {
	if v := os.Getenv("TS_AUTHKEY"); v != "" {
		c.AuthKey = v
	}
	if v := os.Getenv("TSNOTES_STATE_DIR"); v != "" {
		c.StateDir = v
	}
	if v := os.Getenv("TSNOTES_HOSTNAME"); v != "" {
		c.Hostname = v
	}
	if v := os.Getenv("TSNOTES_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	// systemd's LoadCredential puts secrets in a directory rather than the
	// environment, which keeps them out of /proc/<pid>/environ.
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" && c.AuthKey == "" {
		if b, err := os.ReadFile(filepath.Join(dir, "ts_authkey")); err == nil {
			c.AuthKey = strings.TrimSpace(string(b))
		}
	}
}

var validLogLevels = []string{"debug", "info", "warn", "error"}

// Validate reports every problem it can find before anything is started, so a
// misconfiguration fails at launch rather than on the first request.
func (c *Config) Validate() error {
	var errs []error

	switch {
	case c.Hostname == "":
		errs = append(errs, errors.New("hostname must not be empty"))
	case strings.ContainsAny(c.Hostname, ". /\\:"):
		errs = append(errs, fmt.Errorf("hostname %q must be a bare name, not a domain or path", c.Hostname))
	}
	if c.StateDir == "" {
		errs = append(errs, errors.New("stateDir must not be empty"))
	}
	if !slices.Contains(validLogLevels, c.LogLevel) {
		errs = append(errs, fmt.Errorf("logLevel %q must be one of %s", c.LogLevel, strings.Join(validLogLevels, ", ")))
	}
	if c.CacheTTL < 0 {
		errs = append(errs, errors.New("cacheTTL must not be negative"))
	}

	seen := map[string]bool{}
	for i, a := range c.Agents {
		switch {
		case a.Tag == "":
			errs = append(errs, fmt.Errorf("agents[%d]: tag must not be empty", i))
		case !strings.HasPrefix(a.Tag, "tag:"):
			errs = append(errs, fmt.Errorf("agents[%d]: tag %q must start with \"tag:\"", i, a.Tag))
		case seen[a.Tag]:
			errs = append(errs, fmt.Errorf("agents[%d]: duplicate tag %q", i, a.Tag))
		}
		seen[a.Tag] = true
		if a.ActAs == "" {
			errs = append(errs, fmt.Errorf("agents[%d]: actAs must name a tailnet login", i))
		}
	}
	if c.DevAddr != "" && c.DevLogin == "" {
		errs = append(errs, errors.New("devAddr requires devLogin, since a local listener has no tailnet peer to identify"))
	}
	return errors.Join(errs...)
}

// AgentMap flattens the agent list for the identity resolver.
func (c *Config) AgentMap() map[string]string {
	if len(c.Agents) == 0 {
		return nil
	}
	m := make(map[string]string, len(c.Agents))
	for _, a := range c.Agents {
		m[a.Tag] = a.ActAs
	}
	return m
}

// DatabasePath is where SQLite lives.
func (c *Config) DatabasePath() string { return filepath.Join(c.StateDir, "tsnotes.db") }

// VaultsDir contains one directory per user.
func (c *Config) VaultsDir() string { return filepath.Join(c.StateDir, "vaults") }

// TSNetDir holds the tsnet node's own state, including its node key.
func (c *Config) TSNetDir() string { return filepath.Join(c.StateDir, "tsnet") }

// String renders the config for logging with the auth key redacted.
func (c Config) String() string {
	c.AuthKey = ""
	redacted := struct {
		Config
		AuthKey string `json:"authKey"`
	}{Config: c, AuthKey: "(set via TS_AUTHKEY)"}

	b, err := json.Marshal(redacted, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Sprintf("config: %v", err)
	}
	return string(b)
}
