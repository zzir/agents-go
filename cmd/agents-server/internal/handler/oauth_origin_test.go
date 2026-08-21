package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The OAuth redirect_uri origin comes from --base-url when configured, else
// the direct connection. Forwarding headers must be ignored either way: a
// direct client can forge them, and the trusted-deployment answer is base-url.
func TestExternalOrigin(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
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
			name:    "base-url wins over everything",
			baseURL: "https://agents.example.com",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"X-Forwarded-Proto": "http", "X-Forwarded-Host": "evil.example.com"},
			want:    "https://agents.example.com",
		},
		{
			name:    "x-forwarded is not trusted",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.example.com"},
			want:    "http://10.0.0.5:9527",
		},
		{
			name:    "rfc7239 forwarded is not trusted",
			host:    "10.0.0.5:9527",
			headers: map[string]string{"Forwarded": `for=1.2.3.4;host=evil.example.com;proto=https`},
			want:    "http://10.0.0.5:9527",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &McpServerHandler{baseURL: tc.baseURL}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := h.externalOrigin(req); got != tc.want {
				t.Errorf("externalOrigin() = %q, want %q", got, tc.want)
			}
		})
	}
}
