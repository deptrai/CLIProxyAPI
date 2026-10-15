package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestSearXNGClientSearchMockAPI(t *testing.T) {
	var gotQuery, gotFormat string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		gotQuery = q.Get("q")
		gotFormat = q.Get("format")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query": "北京天气",
			"answers": ["明天晴。"],
			"results": [
				{"title": "Example Weather", "url": "https://example.com/w", "content": "snippet one"},
				{"title": "Second", "url": "https://example.com/2", "content": "snippet two"}
			]
		}`))
	}))
	defer server.Close()

	client := newSearXNGClientWithOptions([]string{server.URL}, server.Client())
	hits, answer, errSearch := client.search(context.Background(), "北京天气", 1)
	if errSearch != nil {
		t.Fatalf("search() error = %v", errSearch)
	}
	if gotQuery != "北京天气" {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotFormat != "json" {
		t.Fatalf("format = %q", gotFormat)
	}
	if answer != "明天晴。" {
		t.Fatalf("answer = %q", answer)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/w" {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestSearXNGClientFailoverAcrossURLs(t *testing.T) {
	var dead, live int
	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dead++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer deadServer.Close()
	liveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		live++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":["ok"],"results":[{"title":"T","url":"https://t.example","content":"c"}]}`))
	}))
	defer liveServer.Close()

	client := newSearXNGClientWithOptions([]string{deadServer.URL, liveServer.URL}, http.DefaultClient)
	hits, answer, errSearch := client.search(context.Background(), "q", 5)
	if errSearch != nil {
		t.Fatalf("search() error = %v", errSearch)
	}
	if dead != 1 || live != 1 {
		t.Fatalf("dead=%d live=%d, want 1/1", dead, live)
	}
	if answer != "ok" || len(hits) != 1 {
		t.Fatalf("answer=%q hits=%#v", answer, hits)
	}
	// Next call starts rotation at the live URL; dead server must not be hit again.
	if _, _, err := client.search(context.Background(), "q", 5); err != nil {
		t.Fatal(err)
	}
	if dead != 1 || live != 2 {
		t.Fatalf("after rotation dead=%d live=%d", dead, live)
	}
}

func TestSearXNGClientBasicAuthFromURL(t *testing.T) {
	var gotUser, gotPass string
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = true
		gotUser, gotPass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := newSearXNGClient([]string{"http://cliproxy:secret@" + strings.TrimPrefix(server.URL, "http://")})
	if _, _, err := client.search(context.Background(), "q", 5); err != nil {
		t.Fatal(err)
	}
	if !sawAuth || gotUser != "cliproxy" || gotPass != "secret" {
		t.Fatalf("basic auth = %q/%q (sent=%v)", gotUser, gotPass, sawAuth)
	}
}

func TestSearXNGClientSearchEmptyURL(t *testing.T) {
	client := newSearXNGClient(nil)
	_, _, err := client.search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "searxng_url") {
		t.Fatalf("err = %v", err)
	}
}

func TestSearXNGClientSearchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"format json not enabled"}`))
	}))
	defer server.Close()
	client := newSearXNGClientWithOptions([]string{server.URL}, server.Client())
	_, _, err := client.search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestSearXNGClientTrailingSlash(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := newSearXNGClientWithOptions([]string{server.URL + "/"}, server.Client())
	if _, _, err := client.search(context.Background(), "q", 5); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/search" {
		t.Fatalf("path = %q, want /search", gotPath)
	}
}

func TestRunSearXNGClaudeStreamWithMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": ["2026年6月16日北京多雨。"],
			"results": [
				{"title": "bjmy.gov.cn", "url": "https://www.bjmy.gov.cn/x", "content": "预报"}
			]
		}`))
	}))
	defer server.Close()

	claudeBody := []byte(`{
		"model": "claude-sonnet-4-6",
		"stream": true,
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Perform a web search for the query: 北京天气 2026年6月16日"}]}]
	}`)
	client := newSearXNGClientWithOptions([]string{server.URL}, server.Client())
	payload, headers, errRun := runSearXNGClaudeStreamWithClient(context.Background(), pluginapi.ExecutorRequest{
		Model:           "claude-sonnet-4-6",
		Stream:          true,
		OriginalRequest: claudeBody,
	}, client)
	if errRun != nil {
		t.Fatalf("runSearXNGClaudeStreamWithClient() error = %v", errRun)
	}
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type = %q", headers.Get("Content-Type"))
	}
	text := string(payload)
	for _, needle := range []string{
		"event: message_start",
		`"type":"server_tool_use"`,
		`"name":"web_search"`,
		`"type":"web_search_tool_result"`,
		`"type":"web_search_result"`,
		`https://www.bjmy.gov.cn/x`,
		`"web_search_requests":1`,
		"event: message_stop",
		"北京天气 2026年6月16日",
		"2026年6月16日北京多雨",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("SSE missing %q in:\n%s", needle, text)
		}
	}
}

func TestRunSearXNGClaudeJSONWithMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":["ok"],"results":[{"title":"T","url":"https://t.example","content":"c"}]}`))
	}))
	defer server.Close()

	claudeBody := []byte(`{
		"tools": [{"type": "web_search_20250305", "name": "web_search"}],
		"messages": [{"role": "user", "content": "Perform a web search for the query: test query"}]
	}`)
	client := newSearXNGClientWithOptions([]string{server.URL}, server.Client())
	payload, _, errRun := runSearXNGClaudeWithClient(context.Background(), pluginapi.ExecutorRequest{
		Model:           "claude-sonnet-4-6",
		OriginalRequest: claudeBody,
	}, client)
	if errRun != nil {
		t.Fatal(errRun)
	}
	root := gjson.ParseBytes(payload)
	if root.Get("type").String() != "message" {
		t.Fatalf("type = %s", root.Get("type").String())
	}
	if root.Get("content.0.type").String() != "server_tool_use" {
		t.Fatalf("content.0 = %s", root.Get("content.0.type").String())
	}
	if root.Get("content.1.type").String() != "web_search_tool_result" {
		t.Fatalf("content.1 = %s", root.Get("content.1.type").String())
	}
	if root.Get("content.2.text").String() != "ok" {
		t.Fatalf("text = %s", root.Get("content.2.text").String())
	}
	if root.Get("usage.server_tool_use.web_search_requests").Int() != 1 {
		t.Fatalf("web_search_requests = %d", root.Get("usage.server_tool_use.web_search_requests").Int())
	}
}

func TestSearXNGClientOptionsPassthrough(t *testing.T) {
	var gotLang, gotRange, gotSafe, gotEngines, gotCats string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotLang, gotRange, gotSafe = q.Get("language"), q.Get("time_range"), q.Get("safesearch")
		gotEngines, gotCats = q.Get("engines"), q.Get("categories")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := newSearXNGClientWithOptions([]string{server.URL}, server.Client())
	safe := 1
	opts := searchOptions{Language: "vi", TimeRange: "week", SafeSearch: &safe, Engines: "google,bing", Categories: "general"}
	if _, _, errSearch := client.searchOpts(context.Background(), "q", 5, opts); errSearch != nil {
		t.Fatalf("searchOpts() error = %v", errSearch)
	}
	if gotLang != "vi" || gotRange != "week" || gotSafe != "1" || gotEngines != "google,bing" || gotCats != "general" {
		t.Fatalf("params = lang:%q range:%q safe:%q engines:%q cats:%q", gotLang, gotRange, gotSafe, gotEngines, gotCats)
	}
}

func TestSearXNGClientOptionsOmittedAndAuto(t *testing.T) {
	var gotLang, gotRange, gotSafe, gotEngines, gotCats string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotLang, gotRange, gotSafe = q.Get("language"), q.Get("time_range"), q.Get("safesearch")
		gotEngines, gotCats = q.Get("engines"), q.Get("categories")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	client := newSearXNGClientWithOptions([]string{server.URL}, server.Client())
	if _, _, errSearch := client.searchOpts(context.Background(), "q", 5, searchOptions{Language: "auto"}); errSearch != nil {
		t.Fatalf("searchOpts() error = %v", errSearch)
	}
	if gotLang != "" || gotRange != "" || gotSafe != "" || gotEngines != "" || gotCats != "" {
		t.Fatalf("expected all optional params omitted, got lang:%q range:%q safe:%q engines:%q cats:%q", gotLang, gotRange, gotSafe, gotEngines, gotCats)
	}
}

func TestSearXNGLastBackendHostStripsCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	authed := "http://user:secret@" + strings.TrimPrefix(server.URL, "http://")
	client := newSearXNGClientWithOptions([]string{authed}, server.Client())
	if _, _, errSearch := client.search(context.Background(), "q", 5); errSearch != nil {
		t.Fatalf("search() error = %v", errSearch)
	}
	host := client.lastBackendHost()
	if host == "" || strings.Contains(host, "secret") || strings.Contains(host, "user:") {
		t.Fatalf("lastBackendHost() = %q, want bare host without credentials", host)
	}
	if host != strings.TrimPrefix(server.URL, "http://") {
		t.Fatalf("lastBackendHost() = %q, want %q", host, strings.TrimPrefix(server.URL, "http://"))
	}
}
