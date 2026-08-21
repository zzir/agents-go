package protocol

// UserInfo identifies the authenticated caller: the /auth/me response shape,
// and what the auth middleware attaches to the request for handlers to read.
type UserInfo struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role"`
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
