package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const webSearchAPIModel = "web-search"

// searchOptions carries optional search modifiers understood by the raw
// search API; backends apply what they support and ignore the rest.
type searchOptions struct {
	Language   string
	TimeRange  string
	SafeSearch *int
	Engines    string
	Categories string
}

type rawSearchAPIRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
	Language   string `json:"language,omitempty"`
	TimeRange  string `json:"time_range,omitempty"`
	SafeSearch *int   `json:"safesearch,omitempty"`
	Engines    string `json:"engines,omitempty"`
	Categories string `json:"categories,omitempty"`
}

type rawSearchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

type rawSearchResponse struct {
	Query         string         `json:"query"`
	Answer        string         `json:"answer,omitempty"`
	Backend       string         `json:"backend"`
	BackendDetail string         `json:"backend_detail,omitempty"`
	Results       []rawSearchHit `json:"results"`
}

var validTimeRanges = map[string]bool{"day": true, "week": true, "month": true, "year": true}

// isRawSearchAPIBody reports whether the payload is a raw search API request
// ({"query": "..."} without the claude messages/tools envelope).
func isRawSearchAPIBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe struct {
		Query    string          `json:"query"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if errUnmarshal := json.Unmarshal(body, &probe); errUnmarshal != nil {
		return false
	}
	return strings.TrimSpace(probe.Query) != "" && len(probe.Messages) == 0 && len(probe.Tools) == 0
}

func isRawSearchAPIRequest(req pluginapi.ExecutorRequest) bool {
	return isRawSearchAPIBody(claudeRequestBody(req))
}

// rawSearchBackends lists in-plugin search backends that return raw results, in config order.
func rawSearchBackends(cfg pluginConfig) []routeBackend {
	backends := make([]routeBackend, 0, 3)
	if newSearXNGClient(cfg.searxngURLs()).available() {
		backends = append(backends, backendSearXNG)
	}
	if newTavilyClient(cfg.TavilyAPIKeys).available() {
		backends = append(backends, backendTavily)
	}
	if newBraveClient(cfg.BraveAPIKeys).available() {
		backends = append(backends, backendBrave)
	}
	return backends
}

// runRawSearchAPI executes the raw search API request over the in-plugin search
// backends with penalty ordering and failover, returning plain JSON results.
func runRawSearchAPI(ctx context.Context, req pluginapi.ExecutorRequest) ([]byte, error) {
	cfg := loadedConfig()
	var apiReq rawSearchAPIRequest
	if errUnmarshal := json.Unmarshal(claudeRequestBody(req), &apiReq); errUnmarshal != nil {
		return nil, fmt.Errorf("decode web search api request: %w", errUnmarshal)
	}
	query := strings.TrimSpace(apiReq.Query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	maxResults := apiReq.MaxResults
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 20 {
		maxResults = 20
	}
	if apiReq.TimeRange != "" && !validTimeRanges[apiReq.TimeRange] {
		return nil, fmt.Errorf("time_range must be one of day|week|month|year")
	}
	if apiReq.SafeSearch != nil && (*apiReq.SafeSearch < 0 || *apiReq.SafeSearch > 2) {
		return nil, fmt.Errorf("safesearch must be 0, 1 or 2")
	}
	opts := searchOptions{
		Language:   strings.TrimSpace(apiReq.Language),
		TimeRange:  apiReq.TimeRange,
		SafeSearch: apiReq.SafeSearch,
		Engines:    strings.TrimSpace(apiReq.Engines),
		Categories: strings.TrimSpace(apiReq.Categories),
	}
	backends := rawSearchBackends(cfg)
	if len(backends) == 0 {
		return nil, fmt.Errorf("web search api: no search backend configured")
	}
	searxng := newSearXNGClient(cfg.searxngURLs())
	searchers := map[routeBackend]func(context.Context, string, int, searchOptions) ([]claudeWebSearchHit, string, error){
		backendSearXNG: searxng.searchOpts,
		backendTavily:  newTavilyClient(cfg.TavilyAPIKeys).searchOpts,
		backendBrave:   newBraveClient(cfg.BraveAPIKeys).searchOpts,
	}
	var lastErr error
	for _, backend := range sortBackendsByPenalty(backends) {
		hits, answer, errSearch := searchers[backend](ctx, query, maxResults, opts)
		if errSearch != nil {
			lastErr = errSearch
			continue
		}
		recordBackendSuccess(backend)
		resp := rawSearchResponse{
			Query:   query,
			Answer:  answer,
			Backend: string(backend),
			Results: make([]rawSearchHit, 0, len(hits)),
		}
		if backend == backendSearXNG {
			resp.BackendDetail = searxng.lastBackendHost()
		}
		for _, h := range hits {
			resp.Results = append(resp.Results, rawSearchHit{Title: h.Title, URL: h.URL, Snippet: h.Snippet})
		}
		return json.Marshal(resp)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("web search api: all backends failed")
}
