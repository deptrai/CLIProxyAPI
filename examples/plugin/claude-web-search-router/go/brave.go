package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// braveSearchURL is a var to allow tests to point the client at a mock server.
var braveSearchURL = "https://api.search.brave.com/res/v1/web/search"

type braveClient struct {
	keys    []string
	idx     atomic.Uint64
	http    *http.Client
	baseURL string // empty → https://api.search.brave.com/res/v1/web/search
}

func newBraveClient(keys []string) *braveClient {
	return newBraveClientWithOptions(keys, nil, "")
}

func newBraveClientWithOptions(keys []string, httpClient *http.Client, baseURL string) *braveClient {
	trimmed := make([]string, 0, len(keys))
	for _, key := range keys {
		if k := strings.TrimSpace(key); k != "" {
			trimmed = append(trimmed, k)
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &braveClient{
		keys:    trimmed,
		http:    httpClient,
		baseURL: strings.TrimSpace(baseURL),
	}
}

func (c *braveClient) searchEndpoint() string {
	if c != nil && c.baseURL != "" {
		return c.baseURL
	}
	return braveSearchURL
}

func (c *braveClient) available() bool {
	return c != nil && len(c.keys) > 0
}

func (c *braveClient) nextKey() string {
	if len(c.keys) == 0 {
		return ""
	}
	n := c.idx.Add(1)
	return c.keys[int(n-1)%len(c.keys)]
}

// braveFreshness maps the generic time_range values to Brave's freshness codes.
func braveFreshness(timeRange string) string {
	switch timeRange {
	case "day":
		return "pd"
	case "week":
		return "pw"
	case "month":
		return "pm"
	case "year":
		return "py"
	default:
		return ""
	}
}

// braveSafeSearch maps SearXNG-style 0|1|2 safesearch levels to Brave's
// off|moderate|strict values.
func braveSafeSearch(level int) string {
	switch level {
	case 1:
		return "moderate"
	case 2:
		return "strict"
	default:
		return "off"
	}
}

type braveSearchResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (c *braveClient) search(ctx context.Context, query string, maxResults int) ([]claudeWebSearchHit, string, error) {
	return c.searchOpts(ctx, query, maxResults, searchOptions{})
}

func (c *braveClient) searchOpts(ctx context.Context, query string, maxResults int, opts searchOptions) ([]claudeWebSearchHit, string, error) {
	if !c.available() {
		return nil, "", fmt.Errorf("brave_api_keys is empty")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, "", fmt.Errorf("web search query is empty")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 20 {
		maxResults = 20
	}
	v := url.Values{"q": {query}, "count": {fmt.Sprintf("%d", maxResults)}}
	if opts.Language != "" && opts.Language != "auto" {
		v.Set("search_lang", opts.Language)
	}
	if f := braveFreshness(opts.TimeRange); f != "" {
		v.Set("freshness", f)
	}
	if opts.SafeSearch != nil {
		v.Set("safesearch", braveSafeSearch(*opts.SafeSearch))
	}
	endpoint := c.searchEndpoint() + "?" + v.Encode()
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errNew != nil {
		return nil, "", errNew
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", c.nextKey())
	resp, errDo := c.http.Do(req)
	if errDo != nil {
		return nil, "", errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, "", errRead
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("brave http %d: %s", resp.StatusCode, truncate(string(body), 512))
	}
	var parsed braveSearchResponse
	if errDecode := json.Unmarshal(body, &parsed); errDecode != nil {
		return nil, "", errDecode
	}
	hits := make([]claudeWebSearchHit, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		hits = append(hits, claudeWebSearchHit{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: strings.TrimSpace(r.Description),
		})
	}
	return hits, "", nil
}
