package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// the OAuth redirect_uri origin must honor the reverse-proxy forwarding
// headers, since behind a TLS-terminating proxy the request reaches us as plain
// http on an internal host. The redirect_uri has to match what the browser
// actually loaded or the authorization server rejects the callback.
func TestExternalOrigin(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		want    string
	}{
		{
			name: "direct http",
			host: "localhost:9527",
			want: "http://localhost:9527",
		},
		{
			name:    "x-forwarded",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "agents.example.com"},
			want:    "https://agents.example.com",
		},
		{
			name:    "x-forwarded proxy chain takes first hop",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"X-Forwarded-Proto": "https, http", "X-Forwarded-Host": "agents.example.com, internal"},
			want:    "https://agents.example.com",
		},
		{
			name:    "rfc7239 forwarded wins over direct",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"Forwarded": `for=1.2.3.4;host=agents.example.com;proto=https`},
			want:    "https://agents.example.com",
		},
		{
			name:    "rfc7239 forwarded quoted host",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"Forwarded": `proto=https;host="agents.example.com:8443"`},
			want:    "https://agents.example.com:8443",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := externalOrigin(req); got != tc.want {
				t.Errorf("externalOrigin() = %q, want %q", got, tc.want)
			}
		})
	}
}
