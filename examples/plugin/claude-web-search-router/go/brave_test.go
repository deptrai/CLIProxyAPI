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

func TestBraveClientSearchMockAPI(t *testing.T) {
	var gotQuery, gotCount, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		gotQuery = q.Get("q")
		gotCount = q.Get("count")
		gotToken = r.Header.Get("X-Subscription-Token")
		if accept := r.Header.Get("Accept"); !strings.Contains(accept, "application/json") {
			t.Errorf("accept = %q", accept)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"web": {
				"results": [
					{"title": "Example Weather", "url": "https://example.com/w", "description": "snippet one"}
				]
			}
		}`))
	}))
	defer server.Close()

	client := newBraveClientWithOptions([]string{"bsa-test-key"}, server.Client(), server.URL)
	hits, answer, errSearch := client.search(context.Background(), "北京天气", 3)
	if errSearch != nil {
		t.Fatalf("search() error = %v", errSearch)
	}
	if gotQuery != "北京天气" {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotCount != "3" {
		t.Fatalf("count = %q", gotCount)
	}
	if gotToken != "bsa-test-key" {
		t.Fatalf("subscription token = %q", gotToken)
	}
	if answer != "" {
		t.Fatalf("answer = %q, want empty", answer)
	}
	if len(hits) != 1 || hits[0].URL != "https://example.com/w" || hits[0].Snippet != "snippet one" {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestBraveClientSearchEmptyKeys(t *testing.T) {
	client := newBraveClient(nil)
	_, _, err := client.search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "brave_api_keys") {
		t.Fatalf("err = %v", err)
	}
}

func TestBraveClientSearchHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	defer server.Close()
	client := newBraveClientWithOptions([]string{"bad"}, server.Client(), server.URL)
	_, _, err := client.search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestBraveClientRoundRobinKeys(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-Subscription-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer server.Close()
	client := newBraveClientWithOptions([]string{"k1", "k2"}, server.Client(), server.URL)
	for i := 0; i < 4; i++ {
		if _, _, err := client.search(context.Background(), "q", 1); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 4 || keys[0] != "k1" || keys[1] != "k2" || keys[2] != "k1" || keys[3] != "k2" {
		t.Fatalf("key rotation = %v", keys)
	}
}

func TestBraveClientCountCappedAt20(t *testing.T) {
	var gotCount string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCount = r.URL.Query().Get("count")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer server.Close()
	client := newBraveClientWithOptions([]string{"k"}, server.Client(), server.URL)
	if _, _, err := client.search(context.Background(), "q", 50); err != nil {
		t.Fatal(err)
	}
	if gotCount != "20" {
		t.Fatalf("count = %q, want 20", gotCount)
	}
}

func TestRunBraveClaudeStreamWithMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"web": {"results": [
				{"title": "bjmy.gov.cn", "url": "https://www.bjmy.gov.cn/x", "description": "预报"}
			]}
		}`))
	}))
	defer server.Close()

	claudeBody := []byte(`{
		"model": "claude-sonnet-4-6",
		"stream": true,
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Perform a web search for the query: 北京天气 2026年6月16日"}]}]
	}`)
	client := newBraveClientWithOptions([]string{"bsa-mock"}, server.Client(), server.URL)
	payload, headers, errRun := runBraveClaudeStreamWithClient(context.Background(), pluginapi.ExecutorRequest{
		Model:           "claude-sonnet-4-6",
		Stream:          true,
		OriginalRequest: claudeBody,
	}, client)
	if errRun != nil {
		t.Fatalf("runBraveClaudeStreamWithClient() error = %v", errRun)
	}
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type = %q", headers.Get("Content-Type"))
	}
	text := string(payload)
	for _, needle := range []string{
		"event: message_start",
		`"type":"web_search_tool_result"`,
		`https://www.bjmy.gov.cn/x`,
		"event: message_stop",
		"北京天气 2026年6月16日",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("SSE missing %q in:\n%s", needle, text)
		}
	}
}

func TestRunBraveClaudeJSONWithMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"T","url":"https://t.example","description":"c"}]}}`))
	}))
	defer server.Close()

	claudeBody := []byte(`{
		"tools": [{"type": "web_search_20250305", "name": "web_search"}],
		"messages": [{"role": "user", "content": "Perform a web search for the query: test query"}]
	}`)
	client := newBraveClientWithOptions([]string{"k"}, server.Client(), server.URL)
	payload, _, errRun := runBraveClaudeWithClient(context.Background(), pluginapi.ExecutorRequest{
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
	if root.Get("content.1.type").String() != "web_search_tool_result" {
		t.Fatalf("content.1 = %s", root.Get("content.1.type").String())
	}
	if !strings.Contains(root.Get("content.2.text").String(), "https://t.example") {
		t.Fatalf("text = %s", root.Get("content.2.text").String())
	}
	if root.Get("usage.server_tool_use.web_search_requests").Int() != 1 {
		t.Fatalf("web_search_requests = %d", root.Get("usage.server_tool_use.web_search_requests").Int())
	}
}

func TestBraveClientOptionsMapping(t *testing.T) {
	var gotLang, gotFreshness, gotSafe string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotLang, gotFreshness, gotSafe = q.Get("search_lang"), q.Get("freshness"), q.Get("safesearch")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"T","url":"https://e.com","description":"d"}]}}`))
	}))
	defer server.Close()

	prev := braveSearchURL
	braveSearchURL = server.URL
	defer func() { braveSearchURL = prev }()

	client := newBraveClientWithOptions([]string{"k"}, server.Client(), server.URL)
	safe := 1
	opts := searchOptions{Language: "vi", TimeRange: "day", SafeSearch: &safe}
	if _, _, errSearch := client.searchOpts(context.Background(), "q", 5, opts); errSearch != nil {
		t.Fatalf("searchOpts() error = %v", errSearch)
	}
	if gotLang != "vi" || gotFreshness != "pd" || gotSafe != "moderate" {
		t.Fatalf("params = lang:%q freshness:%q safe:%q", gotLang, gotFreshness, gotSafe)
	}
}
