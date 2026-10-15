package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestIsRawSearchAPIBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"raw query", `{"query":"hello"}`, true},
		{"raw with max_results", `{"query":"hello","max_results":3}`, true},
		{"claude messages", `{"messages":[{"role":"user","content":"q"}],"tools":[{"type":"web_search_20250305"}]}`, false},
		{"tools only", `{"tools":[{"type":"web_search_20250305"}]}`, false},
		{"empty query", `{"query":"  "}`, false},
		{"invalid json", `not-json`, false},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		if got := isRawSearchAPIBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: isRawSearchAPIBody = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRunRawSearchAPIWithMock(t *testing.T) {
	resetBackendPenaltiesForTest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": ["SearXNG is a meta search engine."],
			"results": [
				{"title": "SearXNG", "url": "https://searxng.org", "content": "open source metasearch"},
				{"title": "Docs", "url": "https://docs.searxng.org", "content": "admin docs"}
			]
		}`))
	}))
	defer server.Close()

	currentConfig.Store(pluginConfig{
		Route:       string(backendSearXNG),
		SearXNGURLs: []string{server.URL},
	})
	payload, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"what is searxng","max_results":2}`),
	})
	if errRun != nil {
		t.Fatalf("runRawSearchAPI() error = %v", errRun)
	}
	text := string(payload)
	for _, needle := range []string{
		`"query":"what is searxng"`,
		`"backend":"searxng"`,
		`"title":"SearXNG"`,
		`"url":"https://searxng.org"`,
		`"snippet":"open source metasearch"`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("raw response missing %q in:\n%s", needle, text)
		}
	}
}

func TestRunRawSearchAPIFailoverToBrave(t *testing.T) {
	resetBackendPenaltiesForTest()
	deadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer deadServer.Close()
	liveServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"B","url":"https://b.example","description":"brave snippet"}]}}`))
	}))
	defer liveServer.Close()

	currentConfig.Store(pluginConfig{
		Route:        string(backendSearXNG),
		SearXNGURLs:  []string{deadServer.URL},
		BraveAPIKeys: []string{"k"},
	})
	// Brave client uses the fixed public endpoint; point it at the mock via baseURL.
	oldBraveURL := braveSearchURL
	braveSearchURL = liveServer.URL
	defer func() { braveSearchURL = oldBraveURL }()

	payload, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"q"}`),
	})
	if errRun != nil {
		t.Fatalf("runRawSearchAPI() error = %v", errRun)
	}
	if !strings.Contains(string(payload), `"backend":"brave"`) || !strings.Contains(string(payload), "brave snippet") {
		t.Fatalf("expected brave failover result, got: %s", payload)
	}
}

func TestRunRawSearchAPINoBackend(t *testing.T) {
	resetBackendPenaltiesForTest()
	currentConfig.Store(pluginConfig{Route: string(backendSearXNG)})
	_, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"q"}`),
	})
	if errRun == nil || !strings.Contains(errRun.Error(), "no search backend") {
		t.Fatalf("err = %v", errRun)
	}
}

func TestRunRawSearchAPIMaxResultsCap(t *testing.T) {
	resetBackendPenaltiesForTest()
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	currentConfig.Store(pluginConfig{
		Route:       string(backendSearXNG),
		SearXNGURLs: []string{server.URL},
	})
	if _, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"cap-test","max_results":99}`),
	}); errRun != nil {
		t.Fatal(errRun)
	}
	if gotQuery != "cap-test" {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestRunRawSearchAPIOptionsForwarded(t *testing.T) {
	resetBackendPenaltiesForTest()
	var gotLang, gotRange, gotSafe, gotEngines, gotCats string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotLang, gotRange, gotSafe = q.Get("language"), q.Get("time_range"), q.Get("safesearch")
		gotEngines, gotCats = q.Get("engines"), q.Get("categories")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://e.com","content":"c"}]}`))
	}))
	defer server.Close()

	currentConfig.Store(pluginConfig{
		Route:       string(backendSearXNG),
		SearXNGURLs: []string{server.URL},
	})
	payload, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"q","language":"vi","time_range":"month","safesearch":2,"engines":"google","categories":"news"}`),
	})
	if errRun != nil {
		t.Fatalf("runRawSearchAPI() error = %v", errRun)
	}
	if gotLang != "vi" || gotRange != "month" || gotSafe != "2" || gotEngines != "google" || gotCats != "news" {
		t.Fatalf("params = lang:%q range:%q safe:%q engines:%q cats:%q", gotLang, gotRange, gotSafe, gotEngines, gotCats)
	}
	text := string(payload)
	if !strings.Contains(text, `"backend_detail":"`) {
		t.Fatalf("raw response missing backend_detail in:\n%s", text)
	}
}

func TestRunRawSearchAPIInvalidOptions(t *testing.T) {
	resetBackendPenaltiesForTest()
	currentConfig.Store(pluginConfig{
		Route:       string(backendSearXNG),
		SearXNGURLs: []string{"http://127.0.0.1:1"},
	})
	if _, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"q","time_range":"fortnight"}`),
	}); errRun == nil || !strings.Contains(errRun.Error(), "time_range") {
		t.Fatalf("expected time_range validation error, got %v", errRun)
	}
	if _, errRun := runRawSearchAPI(context.Background(), pluginapi.ExecutorRequest{
		Model:   webSearchAPIModel,
		Payload: []byte(`{"query":"q","safesearch":5}`),
	}); errRun == nil || !strings.Contains(errRun.Error(), "safesearch") {
		t.Fatalf("expected safesearch validation error, got %v", errRun)
	}
}
