package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scottjab/tsnotes/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tsnotes.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	c := config.Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("the defaults must be valid on their own: %v", err)
	}
	if c.Hostname != "tsnotes" {
		t.Errorf("Hostname = %q", c.Hostname)
	}
	if c.CacheTTL <= 0 {
		t.Error("CacheTTL must have a sensible default")
	}
}

func TestLoadFile(t *testing.T) {
	p := writeConfig(t, `{
		"hostname": "notes",
		"stateDir": "/var/lib/tsnotes",
		"logLevel": "debug",
		"cacheTTL": "45s",
		"agents": [{"tag": "tag:notes-agent", "actAs": "alice@github"}]
	}`)

	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Hostname != "notes" {
		t.Errorf("Hostname = %q", c.Hostname)
	}
	if c.StateDir != "/var/lib/tsnotes" {
		t.Errorf("StateDir = %q", c.StateDir)
	}
	if c.CacheTTL.Duration() != 45*time.Second {
		t.Errorf("CacheTTL = %v, want 45s", c.CacheTTL.Duration())
	}
	if len(c.Agents) != 1 || c.Agents[0].Tag != "tag:notes-agent" || c.Agents[0].ActAs != "alice@github" {
		t.Errorf("Agents = %+v", c.Agents)
	}
}

func TestLoadKeepsDefaultsForOmittedFields(t *testing.T) {
	p := writeConfig(t, `{"hostname": "notes"}`)
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheTTL != config.Default().CacheTTL {
		t.Errorf("CacheTTL = %v, want the default to survive a partial config", c.CacheTTL)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	// A silently ignored typo in a config file is the worst kind of bug: it
	// looks like it worked. json/v2's RejectUnknownMembers turns it into an
	// error that names the offending key.
	p := writeConfig(t, `{"hostnamme": "notes"}`)
	_, err := config.Load(p)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "hostnamme") {
		t.Errorf("the error should name the unknown key: %v", err)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	// Running with no config file at all is the normal case.
	c, err := config.Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load of a missing file should fall back to defaults: %v", err)
	}
	if c.Hostname != config.Default().Hostname {
		t.Errorf("Hostname = %q, want the default", c.Hostname)
	}
}

func TestMalformedJSONIsReported(t *testing.T) {
	p := writeConfig(t, `{"hostname": `)
	if _, err := config.Load(p); err == nil {
		t.Error("truncated JSON was accepted")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		wantIn string
	}{
		{"empty hostname", func(c *config.Config) { c.Hostname = "" }, "hostname"},
		{"hostname with a dot", func(c *config.Config) { c.Hostname = "a.b" }, "hostname"},
		{"hostname with a space", func(c *config.Config) { c.Hostname = "a b" }, "hostname"},
		{"empty state dir", func(c *config.Config) { c.StateDir = "" }, "stateDir"},
		{"bad log level", func(c *config.Config) { c.LogLevel = "loud" }, "logLevel"},
		{"negative ttl", func(c *config.Config) { c.CacheTTL = config.Duration(-time.Second) }, "cacheTTL"},
		{"agent without a tag", func(c *config.Config) {
			c.Agents = []config.Agent{{ActAs: "alice@github"}}
		}, "tag"},
		{"agent tag missing the prefix", func(c *config.Config) {
			c.Agents = []config.Agent{{Tag: "notes-agent", ActAs: "alice@github"}}
		}, "tag:"},
		{"agent without a user", func(c *config.Config) {
			c.Agents = []config.Agent{{Tag: "tag:notes-agent"}}
		}, "actAs"},
		{"duplicate agent tag", func(c *config.Config) {
			c.Agents = []config.Agent{
				{Tag: "tag:a", ActAs: "alice@github"},
				{Tag: "tag:a", ActAs: "bob@github"},
			}
		}, "duplicate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config.Default()
			c.StateDir = "/tmp/tsnotes"
			tt.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q should mention %q", err, tt.wantIn)
			}
		})
	}
}

func TestAgentMap(t *testing.T) {
	c := config.Default()
	c.Agents = []config.Agent{
		{Tag: "tag:a", ActAs: "alice@github"},
		{Tag: "tag:b", ActAs: "bob@github"},
	}
	m := c.AgentMap()
	if m["tag:a"] != "alice@github" || m["tag:b"] != "bob@github" {
		t.Errorf("AgentMap = %v", m)
	}
}

func TestDerivedPaths(t *testing.T) {
	c := config.Default()
	c.StateDir = "/var/lib/tsnotes"
	if got := c.DatabasePath(); got != "/var/lib/tsnotes/tsnotes.db" {
		t.Errorf("DatabasePath = %q", got)
	}
	if got := c.VaultsDir(); got != "/var/lib/tsnotes/vaults" {
		t.Errorf("VaultsDir = %q", got)
	}
	if got := c.TSNetDir(); got != "/var/lib/tsnotes/tsnet" {
		t.Errorf("TSNetDir = %q", got)
	}
}

func TestAuthKeyComesFromTheEnvironmentNotTheFile(t *testing.T) {
	// A tailnet auth key is a credential. It has no business sitting in a config
	// file that gets copied around or committed.
	p := writeConfig(t, `{"authKey": "tskey-auth-secret"}`)
	if _, err := config.Load(p); err == nil {
		t.Error("authKey in the config file should be rejected outright")
	}

	t.Setenv("TS_AUTHKEY", "tskey-auth-fromenv")
	c := config.Default()
	c.ApplyEnv()
	if c.AuthKey != "tskey-auth-fromenv" {
		t.Errorf("AuthKey = %q, want it read from TS_AUTHKEY", c.AuthKey)
	}
}

func TestRedactedStringHidesTheAuthKey(t *testing.T) {
	c := config.Default()
	c.AuthKey = "tskey-auth-supersecret"
	if strings.Contains(c.String(), "supersecret") {
		t.Errorf("the auth key leaked into the config's String(): %s", c.String())
	}
}
