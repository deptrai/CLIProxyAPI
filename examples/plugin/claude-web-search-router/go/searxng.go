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

type searxngClient struct {
	urls []string
	idx  atomic.Uint64
	http *http.Client
}

func newSearXNGClient(urls []string) *searxngClient {
	return newSearXNGClientWithOptions(urls, nil)
}

func newSearXNGClientWithOptions(urls []string, httpClient *http.Client) *searxngClient {
	trimmed := make([]string, 0, len(urls))
	for _, raw := range urls {
		u := strings.TrimRight(strings.TrimSpace(raw), "/")
		if u != "" {
			trimmed = append(trimmed, u)
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &searxngClient{urls: trimmed, http: httpClient}
}

func (c *searxngClient) available() bool {
	return c != nil && len(c.urls) > 0
}

type searxngSearchResponse struct {
	Answers []string `json:"answers"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (c *searxngClient) search(ctx context.Context, query string, maxResults int) ([]claudeWebSearchHit, string, error) {
	if !c.available() {
		return nil, "", fmt.Errorf("searxng_urls is empty")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, "", fmt.Errorf("web search query is empty")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	start := int(c.idx.Add(1)-1) % len(c.urls)
	var lastErr error
	for offset := 0; offset < len(c.urls); offset++ {
		base := c.urls[(start+offset)%len(c.urls)]
		hits, answer, err := c.searchOne(ctx, base, query, maxResults)
		if err == nil {
			return hits, answer, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func (c *searxngClient) searchOne(ctx context.Context, baseURL, query string, maxResults int) ([]claudeWebSearchHit, string, error) {
	endpoint := baseURL + "/search?" + url.Values{"q": {query}, "format": {"json"}}.Encode()
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errNew != nil {
		return nil, "", errNew
	}
	req.Header.Set("Accept", "application/json")
	if req.URL.User != nil {
		password, _ := req.URL.User.Password()
		req.SetBasicAuth(req.URL.User.Username(), password)
	}
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
		return nil, "", fmt.Errorf("searxng http %d: %s", resp.StatusCode, truncate(string(body), 512))
	}
	var parsed searxngSearchResponse
	if errDecode := json.Unmarshal(body, &parsed); errDecode != nil {
		return nil, "", errDecode
	}
	if len(parsed.Results) > maxResults {
		parsed.Results = parsed.Results[:maxResults]
	}
	hits := make([]claudeWebSearchHit, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		hits = append(hits, claudeWebSearchHit{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: strings.TrimSpace(r.Content),
		})
	}
	answer := ""
	for _, a := range parsed.Answers {
		if trimmed := strings.TrimSpace(a); trimmed != "" {
			answer = trimmed
			break
		}
	}
	return hits, answer, nil
}
