package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/secrets"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// config is the process configuration: each flag with its environment
// fallback resolved, and whatever a flag can get wrong without a database
// already refused (loadConfig).
type config struct {
	Host string
	Port int
	// DB is the database path or DSN (dbTarget).
	DB        string
	LogLevel  string
	LogFormat string
	// BaseURL is the canonical public origin; "" when --base-url is unset.
	BaseURL        string
	TrustedProxies []string
	// AuditRetentionDays prunes older audit entries; 0 keeps them forever.
	AuditRetentionDays int
	// SecretBox seals stored credentials; nil leaves them in plaintext.
	SecretBox *secrets.Box
	Auth      authConfig
}

// authConfig is the --auth mode and what that mode runs on.
type authConfig struct {
	// Mode is authn.ModeToken or authn.ModeOAuth.
	Mode string
	// Token is token mode's static token; "" has the server generate one.
	Token string
	// The rest is OAuth mode's; BootstrapAdmin is normalized, "" names nobody.
	Providers      []authn.OAuthProvider
	AllowedDomains []string
	AllowedEmails  []string
	BootstrapAdmin string
}

// flagReader reads a command's flags by name. A name the command does not
// define is a bug in this file: it is kept in err, which loadConfig returns.
type flagReader struct {
	cmd *cobra.Command
	err error
}

func (r *flagReader) str(name string) string {
	v, err := r.cmd.Flags().GetString(name)
	r.err = errors.Join(r.err, err)
	return v
}

func (r *flagReader) num(name string) int {
	v, err := r.cmd.Flags().GetInt(name)
	r.err = errors.Join(r.err, err)
	return v
}

// secret is a secret's flag, then the variable that keeps it off argv and ps.
func (r *flagReader) secret(name, env string) string {
	if v := r.str(name); v != "" {
		return v
	}
	return os.Getenv(env)
}

// dbTarget resolves the database argument: an explicit --db wins, then
// AGENTS_DB, then the flag's default.
func dbTarget(flag string, set bool, getenv func(string) string) string {
	if set {
		return flag
	}
	if env := getenv("AGENTS_DB"); env != "" {
		return env
	}
	return flag
}

// loadConfig reads cmd's flags and their environment fallbacks into a config,
// the one place either is read (invariant 105). It opens nothing but
// the key file, so a refusal here has touched no row.
func loadConfig(cmd *cobra.Command) (*config, error) {
	r := &flagReader{cmd: cmd}
	cfg := &config{
		Host:               r.str("host"),
		Port:               r.num("port"),
		DB:                 dbTarget(r.str("db"), cmd.Flags().Changed("db"), os.Getenv),
		LogLevel:           r.str("log-level"),
		LogFormat:          r.str("log-format"),
		TrustedProxies:     settings.SplitList(r.str("trusted-proxies")),
		AuditRetentionDays: r.num("audit-retention-days"),
	}
	var err error
	if raw := r.str("base-url"); raw != "" {
		if cfg.BaseURL, err = server.NormalizeBaseURL(raw); err != nil {
			return nil, err
		}
	}
	if cfg.SecretBox, err = secrets.FromEnvOrFile("AGENTS_SECRET_KEY", r.str("secret-key-file")); err != nil {
		return nil, err
	}
	if cfg.Auth, err = loadAuth(r, cfg.BaseURL); err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	return cfg, nil
}

// loadAuth reads the --auth mode and the flags it runs on, refusing the
// combinations no start can serve.
func loadAuth(r *flagReader, baseURL string) (authConfig, error) {
	token := r.str("token")
	switch mode := r.str("auth"); mode {
	case authn.ModeToken:
		if token == "" {
			token = os.Getenv("AGENTS_TOKEN")
		}
		return authConfig{Mode: mode, Token: token}, nil
	case authn.ModeOAuth:
		if token != "" {
			return authConfig{}, fmt.Errorf("--token cannot combine with --auth oauth; programmatic access uses personal access tokens")
		}
		if baseURL == "" {
			return authConfig{}, fmt.Errorf("--auth oauth requires --base-url: the OAuth redirect URI derives from it")
		}
		var oauthProviders []authn.OAuthProvider
		if id := r.str("oauth-google-client-id"); id != "" {
			secret := r.secret("oauth-google-client-secret", "AGENTS_OAUTH_GOOGLE_CLIENT_SECRET")
			if secret == "" {
				return authConfig{}, fmt.Errorf("google login needs --oauth-google-client-secret (or AGENTS_OAUTH_GOOGLE_CLIENT_SECRET)")
			}
			oauthProviders = append(oauthProviders, &authn.Google{ClientID: id, ClientSecret: secret})
		}
		if id := r.str("oauth-github-client-id"); id != "" {
			secret := r.secret("oauth-github-client-secret", "AGENTS_OAUTH_GITHUB_CLIENT_SECRET")
			if secret == "" {
				return authConfig{}, fmt.Errorf("github login needs --oauth-github-client-secret (or AGENTS_OAUTH_GITHUB_CLIENT_SECRET)")
			}
			oauthProviders = append(oauthProviders, &authn.GitHub{ClientID: id, ClientSecret: secret})
		}
		if len(oauthProviders) == 0 {
			return authConfig{}, fmt.Errorf("--auth oauth needs at least one provider (--oauth-google-client-id or --oauth-github-client-id)")
		}
		domains, emails := settings.SplitList(r.str("allowed-domains")), settings.SplitList(r.str("allowed-emails"))
		rawAdmin := r.str("bootstrap-admin")
		bootstrapAdmin := store.NormalizeEmail(rawAdmin)
		for _, d := range domains {
			if strings.Contains(d, "@") {
				return authConfig{}, fmt.Errorf("--allowed-domains takes domains, not addresses: %q", d)
			}
		}
		for _, e := range append(emails, bootstrapAdmin) {
			if e != "" && !strings.Contains(e, "@") {
				return authConfig{}, fmt.Errorf("--allowed-emails and --bootstrap-admin take addresses: %q", e)
			}
		}
		if rawAdmin != "" && bootstrapAdmin == "" {
			return authConfig{}, fmt.Errorf("--bootstrap-admin is blank")
		}
		if len(domains) == 0 && len(emails) == 0 && bootstrapAdmin == "" {
			return authConfig{}, fmt.Errorf("--auth oauth needs an allowlist: --allowed-domains, --allowed-emails or --bootstrap-admin")
		}
		return authConfig{
			Mode: mode, Providers: oauthProviders,
			AllowedDomains: domains, AllowedEmails: emails, BootstrapAdmin: bootstrapAdmin,
		}, nil
	default:
		return authConfig{}, fmt.Errorf("unknown --auth mode %q (token or oauth)", mode)
	}
}
