package cmd

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
)

// envNames are the variables loadConfig reads.
var envNames = []string{
	"AGENTS_DB", "AGENTS_TOKEN", "AGENTS_SECRET_KEY",
	"AGENTS_OAUTH_GOOGLE_CLIENT_SECRET", "AGENTS_OAUTH_GITHUB_CLIENT_SECRET",
}

// hermeticEnv sets every variable loadConfig reads to env's value, blank when
// env does not name it: the environment the tests run in never leaks in.
func hermeticEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range envNames {
		t.Setenv(name, env[name])
	}
}

// configFor loads the config of one command line under env.
func configFor(t *testing.T, env map[string]string, args ...string) (*config, error) {
	t.Helper()
	hermeticEnv(t, env)
	cmd := newRootCmd()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return loadConfig(cmd)
}

// sealingKey is a well-formed AGENTS_SECRET_KEY: 32 bytes, in hex.
var sealingKey = strings.Repeat("0a", 32)

// Every flag combination no start can serve is refused when the config is
// loaded — before the address is bound or the database opened — in the words
// the server has always used.
func TestLoadConfigRefusesWhatNoStartCanServe(t *testing.T) {
	with := func(base []string, more ...string) []string { return append(slices.Clone(base), more...) }
	oauth := []string{"--auth", "oauth", "--base-url", "https://app.example"}
	google := with(oauth, "--oauth-google-client-id", "gid", "--oauth-google-client-secret", "gsecret")
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
		// prefix: want is where the message starts (the rest is the OS's).
		prefix bool
	}{
		{name: "an unknown mode", args: []string{"--auth", "saml"},
			want: `unknown --auth mode "saml" (token or oauth)`},
		{name: "a static token in oauth mode", args: with(google, "--allowed-domains", "example.com", "--token", "t"),
			want: "--token cannot combine with --auth oauth; programmatic access uses personal access tokens"},
		{name: "oauth without a base URL", args: []string{"--auth", "oauth"},
			want: "--auth oauth requires --base-url: the OAuth redirect URI derives from it"},
		{name: "google without its secret", args: with(oauth, "--oauth-google-client-id", "gid"),
			want: "google login needs --oauth-google-client-secret (or AGENTS_OAUTH_GOOGLE_CLIENT_SECRET)"},
		{name: "github without its secret", args: with(oauth, "--oauth-github-client-id", "hid"),
			want: "github login needs --oauth-github-client-secret (or AGENTS_OAUTH_GITHUB_CLIENT_SECRET)"},
		{name: "oauth with no provider", args: with(oauth, "--allowed-domains", "example.com"),
			want: "--auth oauth needs at least one provider (--oauth-google-client-id or --oauth-github-client-id)"},
		{name: "a secret without its client id is no provider",
			env:  map[string]string{"AGENTS_OAUTH_GOOGLE_CLIENT_SECRET": "gsecret"},
			args: with(oauth, "--allowed-domains", "example.com"),
			want: "--auth oauth needs at least one provider (--oauth-google-client-id or --oauth-github-client-id)"},
		{name: "oauth with no allowlist", args: google,
			want: "--auth oauth needs an allowlist: --allowed-domains, --allowed-emails or --bootstrap-admin"},
		{name: "an address among the domains", args: with(google, "--allowed-domains", "example.com,a@example.com"),
			want: `--allowed-domains takes domains, not addresses: "a@example.com"`},
		{name: "a domain among the emails", args: with(google, "--allowed-emails", "a@example.com,example.com"),
			want: `--allowed-emails and --bootstrap-admin take addresses: "example.com"`},
		{name: "a bootstrap admin that is no address", args: with(google, "--bootstrap-admin", "root"),
			want: `--allowed-emails and --bootstrap-admin take addresses: "root"`},
		{name: "a blank bootstrap admin", args: with(google, "--allowed-domains", "example.com", "--bootstrap-admin", "  "),
			want: "--bootstrap-admin is blank"},
		{name: "a base URL that is not http", args: []string{"--base-url", "ftp://app.example"},
			want: `base-url must be http or https, got "ftp://app.example"`},
		{name: "a base URL with a path", args: []string{"--base-url", "https://app.example/ui"},
			want: `base-url must be a bare scheme://host[:port], got "https://app.example/ui"`},
		{name: "a malformed sealing key", env: map[string]string{"AGENTS_SECRET_KEY": "short"},
			want: "secrets: key must decode (base64 or hex) to 32 bytes"},
		{name: "a key file that is not there", args: []string{"--secret-key-file", filepath.Join(t.TempDir(), "absent")},
			want: "secrets: reading key file: ", prefix: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := configFor(t, c.env, c.args...)
			if err == nil {
				t.Fatalf("loadConfig(%v) = %+v, want it refused: %s", c.args, cfg, c.want)
			}
			if got := err.Error(); got != c.want && (!c.prefix || !strings.HasPrefix(got, c.want)) {
				t.Fatalf("loadConfig(%v):\n got %q\nwant %q", c.args, got, c.want)
			}
		})
	}
}

// A flag wins over its variable, the variable over the default, and what is
// loaded is already in the shape the server uses.
func TestLoadConfigResolvesFlagsAndFallbacks(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := configFor(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := config{
			Host: "127.0.0.1", Port: 9527, DB: "data.db", LogLevel: "info", LogFormat: "text",
			Auth: authConfig{Mode: authn.ModeToken},
		}
		if cfg.Host != want.Host || cfg.Port != want.Port || cfg.DB != want.DB || cfg.LogLevel != want.LogLevel ||
			cfg.LogFormat != want.LogFormat || cfg.BaseURL != "" || len(cfg.TrustedProxies) != 0 ||
			cfg.AuditRetentionDays != 0 || cfg.SecretBox != nil || cfg.Auth.Mode != want.Auth.Mode || cfg.Auth.Token != "" {
			t.Fatalf("defaults = %+v, want %+v", cfg, want)
		}
	})
	t.Run("the variables back the flags", func(t *testing.T) {
		cfg, err := configFor(t, map[string]string{
			"AGENTS_DB": "postgres://env", "AGENTS_TOKEN": "from-env", "AGENTS_SECRET_KEY": sealingKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DB != "postgres://env" || cfg.Auth.Token != "from-env" || cfg.SecretBox == nil {
			t.Fatalf("config = %+v, want the database, token and key from the environment", cfg)
		}
	})
	t.Run("a flag wins over its variable", func(t *testing.T) {
		keyFile := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(keyFile, []byte(sealingKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The variable's key is malformed: only the file's can have been read.
		cfg, err := configFor(t, map[string]string{
			"AGENTS_DB": "postgres://env", "AGENTS_TOKEN": "from-env", "AGENTS_SECRET_KEY": "short",
		}, "--db", "mine.db", "--token", "from-flag", "--secret-key-file", keyFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DB != "mine.db" || cfg.Auth.Token != "from-flag" || cfg.SecretBox == nil {
			t.Fatalf("config = %+v, want the database, token and key from the flags", cfg)
		}
	})
	t.Run("values are normalized", func(t *testing.T) {
		cfg, err := configFor(t, nil,
			"--host", "0.0.0.0", "--port", "8080", "--log-level", "debug", "--log-format", "json",
			"--base-url", "https://app.example/", "--trusted-proxies", " 10.0.0.0/8, ,127.0.0.1",
			"--audit-retention-days", "30")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "0.0.0.0" || cfg.Port != 8080 || cfg.LogLevel != "debug" || cfg.LogFormat != "json" ||
			cfg.BaseURL != "https://app.example" || !slices.Equal(cfg.TrustedProxies, []string{"10.0.0.0/8", "127.0.0.1"}) ||
			cfg.AuditRetentionDays != 30 {
			t.Fatalf("config = %+v", cfg)
		}
	})
	t.Run("oauth", func(t *testing.T) {
		// AGENTS_TOKEN is the --token flag's fallback, and only the flag is
		// refused in oauth mode: a variable left set is ignored.
		cfg, err := configFor(t, map[string]string{
			"AGENTS_TOKEN": "left-over", "AGENTS_OAUTH_GOOGLE_CLIENT_SECRET": "g-env", "AGENTS_OAUTH_GITHUB_CLIENT_SECRET": "h-env",
		}, "--auth", "oauth", "--base-url", "http://localhost:9527",
			"--oauth-google-client-id", "gid", "--oauth-github-client-id", "hid", "--oauth-github-client-secret", "h-flag",
			"--allowed-domains", "example.com, example.org", "--allowed-emails", "a@else.example",
			"--bootstrap-admin", " Root@Example.com ")
		if err != nil {
			t.Fatal(err)
		}
		auth := cfg.Auth
		if auth.Mode != authn.ModeOAuth || auth.Token != "" || auth.BootstrapAdmin != "root@example.com" ||
			!slices.Equal(auth.AllowedDomains, []string{"example.com", "example.org"}) ||
			!slices.Equal(auth.AllowedEmails, []string{"a@else.example"}) || len(auth.Providers) != 2 {
			t.Fatalf("auth = %+v", auth)
		}
		google, okG := auth.Providers[0].(*authn.Google)
		github, okH := auth.Providers[1].(*authn.GitHub)
		if !okG || !okH || google.ClientID != "gid" || google.ClientSecret != "g-env" ||
			github.ClientID != "hid" || github.ClientSecret != "h-flag" {
			t.Fatalf("providers = %+v, want google on its variable's secret then github on its flag's", auth.Providers)
		}
	})
}

// --trusted-proxies is applied to the server, an entry that is neither an IP
// nor a CIDR refused; a --base-url with no proxy trusted is worth a warning.
func TestTrustProxies(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	srv := func() *server.Server { return server.New(slog.New(slog.DiscardHandler), nil, nil) }

	if err := trustProxies(srv(), &config{BaseURL: "https://app.example", TrustedProxies: []string{"10.0.0.0/8", "127.0.0.1"}}, log); err != nil {
		t.Fatalf("an IP and a CIDR: %v", err)
	}
	if err := trustProxies(srv(), &config{}, log); err != nil {
		t.Fatalf("no proxy and no base URL: %v", err)
	}
	if logged.Len() != 0 {
		t.Fatalf("warned with nothing to warn about: %s", logged.String())
	}
	err := trustProxies(srv(), &config{TrustedProxies: []string{"10.0.0.0/8", "proxy.internal"}}, log)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid --trusted-proxies: ") {
		t.Fatalf("a hostname among the proxies: err = %v, want it refused", err)
	}
	if err := trustProxies(srv(), &config{BaseURL: "https://app.example"}, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "--base-url is set but --trusted-proxies is not") {
		t.Fatalf("a base URL with no trusted proxy went unremarked: %q", logged.String())
	}
}
