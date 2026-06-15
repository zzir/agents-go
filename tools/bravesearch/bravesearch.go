// Package bravesearch provides a function tool that searches the web via the
// Brave Search API (https://api.search.brave.com/res/v1/web/search).
//
// The tool is a plain, provider-agnostic agents.FunctionTool: it calls Brave's
// REST API from Go and returns formatted results to the model, so it works with
// any model backend. See the API reference:
// https://api-dashboard.search.brave.com/api-reference/web/search/get
package bravesearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
)

// defaultEndpoint is the public Brave Web Search endpoint.
const defaultEndpoint = "https://api.search.brave.com/res/v1/web/search"

// Options configures the Brave search tool.
type Options struct {
	// APIKey is the Brave Search subscription token. If empty, the value of the
	// BRAVE_API_KEY environment variable is used. New returns an error if both
	// are empty.
	APIKey string

	// Count is the number of results to request (1-20). Defaults to 5; values
	// outside the range are clamped.
	Count int

	// Country, SearchLang, SafeSearch and Freshness are optional Brave query
	// parameters; empty values are omitted so Brave applies its own defaults.
	// SafeSearch is one of "off", "moderate", "strict". Freshness is one of
	// "pd", "pw", "pm", "py", or a "YYYY-MM-DDtoYYYY-MM-DD" range.
	Country    string
	SearchLang string
	SafeSearch string
	Freshness  string

	// HTTPClient performs the request. Defaults to a client with a 30s timeout.
	HTTPClient *http.Client

	// Endpoint overrides the Brave API URL. Defaults to the public endpoint; set
	// it in tests to point at a stub server.
	Endpoint string
}

// New returns a function tool named "brave_search". It returns an error if no
// API key is available (neither Options.APIKey nor BRAVE_API_KEY is set).
func New(opts Options) (*agents.FunctionTool, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("BRAVE_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("bravesearch: no API key (set Options.APIKey or BRAVE_API_KEY)")
	}

	count := opts.Count
	switch {
	case count <= 0:
		count = 5
	case count > 20:
		count = 20
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	c := &caller{client: client, endpoint: endpoint, apiKey: apiKey, count: count, opts: opts}
	return agents.NewFunctionTool("brave_search",
		"Search the web with the Brave Search API and return the top results (title, URL, description).",
		c.run), nil
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"the web search query"`
}

// caller holds the resolved configuration for one tool instance.
type caller struct {
	client   *http.Client
	endpoint string
	apiKey   string
	count    int
	opts     Options
}

func (c *caller) run(ctx context.Context, _ *agents.ToolContext, args searchArgs) (string, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", fmt.Errorf("query must not be empty")
	}

	u, err := url.Parse(c.endpoint)
	if err != nil {
		return "", fmt.Errorf("bravesearch: bad endpoint: %w", err)
	}
	qs := u.Query()
	qs.Set("q", query)
	qs.Set("count", strconv.Itoa(c.count))
	if c.opts.Country != "" {
		qs.Set("country", c.opts.Country)
	}
	if c.opts.SearchLang != "" {
		qs.Set("search_lang", c.opts.SearchLang)
	}
	if c.opts.SafeSearch != "" {
		qs.Set("safesearch", c.opts.SafeSearch)
	}
	if c.opts.Freshness != "" {
		qs.Set("freshness", c.opts.Freshness)
	}
	u.RawQuery = qs.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("bravesearch: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("bravesearch: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bravesearch: API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return formatResults(body)
}

// highlightStripper removes the <strong> markers Brave wraps around query terms
// in result descriptions, leaving plain text for the model.
var highlightStripper = strings.NewReplacer("<strong>", "", "</strong>", "")

func formatResults(body []byte) (string, error) {
	var parsed struct {
		Web *struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("bravesearch: decoding response: %w", err)
	}
	if parsed.Web == nil || len(parsed.Web.Results) == 0 {
		return "No results found.", nil
	}

	var b strings.Builder
	for i, r := range parsed.Web.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if desc := strings.TrimSpace(highlightStripper.Replace(r.Description)); desc != "" {
			fmt.Fprintf(&b, "   %s\n", desc)
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
