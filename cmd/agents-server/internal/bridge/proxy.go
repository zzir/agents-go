package bridge

import (
	"context"
	"net/http"
	"net/url"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProxyHTTPClient returns an *http.Client configured with the proxy_url
// from settings. Returns nil if no proxy is configured.
func ProxyHTTPClient(ctx context.Context, settings *store.SettingStore) *http.Client {
	proxyURL := getProxyURL(ctx, settings)
	if proxyURL == "" {
		return nil
	}
	return newProxyClient(proxyURL)
}

func getProxyURL(ctx context.Context, settings *store.SettingStore) string {
	if settings == nil {
		return ""
	}
	s, err := settings.Get(ctx, "proxy_url")
	if err != nil || s.Value == "" {
		return ""
	}
	return s.Value
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
