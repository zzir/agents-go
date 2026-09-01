package handler

import "testing"

// The persisted OAuth grant is bound to endpoint + auth mode + client id;
// moving any of them must read as an identity change (the update then clears
// the grant), while touching anything else must not.
func TestOAuthIdentityChanged(t *testing.T) {
	base := `{"endpoint":"https://a/mcp","auth_mode":"oauth","oauth_client_id":"c1","oauth_scopes":"s"}`
	for _, tc := range []struct {
		name string
		next string
		want bool
	}{
		{"unchanged", base, false},
		{"endpoint moved", `{"endpoint":"https://b/mcp","auth_mode":"oauth","oauth_client_id":"c1"}`, true},
		{"auth mode moved", `{"endpoint":"https://a/mcp","auth_mode":"header","oauth_client_id":"c1"}`, true},
		{"client id moved", `{"endpoint":"https://a/mcp","auth_mode":"oauth","oauth_client_id":"c2"}`, true},
		{"non-identity field moved", `{"endpoint":"https://a/mcp","auth_mode":"oauth","oauth_client_id":"c1","oauth_scopes":"other","max_retry_attempts":3}`, false},
		{"unparseable keeps the grant", `{`, false},
	} {
		if got := oauthIdentityChanged([]byte(tc.next), []byte(base)); got != tc.want {
			t.Errorf("%s: oauthIdentityChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}
