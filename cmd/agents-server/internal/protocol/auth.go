package protocol

// UserInfo identifies the authenticated caller: the /auth/me response shape,
// and what the auth middleware attaches to the request for handlers to read.
type UserInfo struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
	// AvatarURL is the login provider's picture, when it gave one.
	AvatarURL string `json:"avatar_url,omitempty"`
}

// AuthConfig tells the login page how to authenticate: mode "token" renders
// the token form, mode "oauth" one button per configured provider.
type AuthConfig struct {
	Mode      string   `json:"mode"`
	Providers []string `json:"providers,omitempty"`
}

// AuthSession is a successful /auth/exchange: the session token (this response
// is the only place its plaintext exists) and who it belongs to.
type AuthSession struct {
	Token string   `json:"token"`
	User  UserInfo `json:"user"`
}

// PatView is one personal access token as the list shows it — never the
// secret, which exists only in the PatCreated response that minted it.
type PatView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// PatCreated answers a mint: the plaintext appears here once and is never
// retrievable again.
type PatCreated struct {
	Token string  `json:"token"`
	Pat   PatView `json:"pat"`
}
