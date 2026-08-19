package bridge

import (
	"context"
	"net/http"
	"net/url"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
)

// ProxyHTTPClient returns an *http.Client configured with the proxy_url
// from settings. Returns nil if no proxy is configured.
func ProxyHTTPClient(ctx context.Context, cfg *settings.Reader) *http.Client {
	proxyURL := cfg.String(ctx, settings.KeyProxyURL)
	if proxyURL == "" {
		return nil
	}
	return newProxyClient(proxyURL)
}

func newProxyClient(proxyURL string) *http.Client {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(u),
		},
	}
}
