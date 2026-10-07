package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"gopkg.in/yaml.v3"
)

var currentConfig atomic.Value

type pluginConfig struct {
	MaxEntries  int    `yaml:"max_entries"`
	PersistFile string `yaml:"persist_file"`
}

func configOrDefault() pluginConfig {
	if v := currentConfig.Load(); v != nil {
		if cfg, ok := v.(pluginConfig); ok {
			return cfg
		}
	}
	return pluginConfig{MaxEntries: 500}
}

// attemptView is one upstream try inside a request (failover chain element).
type attemptView struct {
	InputTotal int64  `json:"input_total,omitempty"`
	Provider   string `json:"provider"`
	Model      string `json:"model,omitempty"`
	Account    string `json:"account,omitempty"`
	Status     int    `json:"status"`
	Failed     bool   `json:"failed,omitempty"`
	Error      string `json:"error,omitempty"`
	TI         int64  `json:"ti"`
	TO         int64  `json:"to"`
	Reasoning  int64  `json:"reasoning,omitempty"`
	CacheRead  int64  `json:"cache_read,omitempty"`
	CacheWrite int64  `json:"cache_write,omitempty"`
	TPS        int64  `json:"tps,omitempty"`
	TTFTms     int64  `json:"ttft_ms,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// logEntry is one row in the request log table — one row per client request.
// Per-attempt usage records are folded into Attempts (the fallback chain).
type logEntry struct {
	Running        bool          `json:"running,omitempty"`
	isParent       bool          // created by request intercept; not serialized
	hasUsage       bool          // usage.handle data merged; not serialized
	Attempts       []attemptView `json:"attempts,omitempty"`
	RequestID      string        `json:"id"`
	TraceID        string        `json:"trace_id"`
	Time           int64         `json:"time"` // unix seconds
	Status         int           `json:"status"`
	Outcome        string        `json:"outcome"`
	Error          string        `json:"error,omitempty"`
	Model          string        `json:"model"`
	Requested      string        `json:"requested"`
	ResponseModel  string        `json:"response_model,omitempty"`
	Provider       string        `json:"provider"`
	Executor       string        `json:"executor,omitempty"`
	Account        string        `json:"account"`
	AuthType       string        `json:"auth_type,omitempty"`
	APIKey         string        `json:"api_key"`
	Stream         bool          `json:"stream"`
	TokensIn       int64         `json:"ti"`
	InputTotal     int64         `json:"input_total"`
	TokensOut      int64         `json:"to"`
	Reasoning      int64         `json:"reasoning,omitempty"`
	CacheRead      int64         `json:"cache_read,omitempty"`
	CacheWrite     int64         `json:"cache_write,omitempty"`
	TokensTotal    int64         `json:"total"`
	TPS            int64         `json:"tps"`
	TTFTms         int64         `json:"ttft_ms"`
	DurationMs     int64         `json:"duration_ms"`
	Session        string        `json:"session,omitempty"`
	ParentSession  string        `json:"parent_session,omitempty"`
	Source         string        `json:"source,omitempty"`
	Prompt         string        `json:"prompt,omitempty"` // capped at 12KB
	ReasoningLevel string        `json:"reasoning_level,omitempty"`
	BaseURL        string        `json:"base_url,omitempty"`
}

var store = struct {
	sync.Mutex
	order []*logEntry          // oldest -> newest
	byID  map[string]*logEntry // RequestID -> entry
}{byID: make(map[string]*logEntry)}

var persistOnce sync.Once

// persistAppend appends the latest snapshot of an entry as one JSONL line.
// On load the last line for a RequestID wins.
func persistAppend(e *logEntry) {
	path := configOrDefault().PersistFile
	if path == "" || e == nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Printf("request-logs: persist close failed: %v", errClose)
		}
	}()
	line = append(line, '\n')
	if _, errWrite := f.Write(line); errWrite != nil {
		log.Printf("request-logs: persist write failed: %v", errWrite)
	}
}

// persistTruncate clears the log file when the buffer is cleared.
func persistTruncate() {
	path := configOrDefault().PersistFile
	if path == "" {
		return
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		log.Printf("request-logs: persist truncate failed: %v", err)
	}
}

// persistLoadOnce restores buffered entries from the JSONL file at startup.
// Rows still marked "running" at load were killed mid-flight -> mark stale.
func persistLoadOnce() {
	persistOnce.Do(func() {
		path := configOrDefault().PersistFile
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		maxEntries := configOrDefault().MaxEntries
		byID := map[string]*logEntry{}
		var order []string
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var e logEntry
			if json.Unmarshal(line, &e) != nil || e.RequestID == "" {
				continue
			}
			if e.Outcome == "running" || e.Running {
				e.Outcome = "stale"
				e.Running = false
				e.Error = "process restarted mid-request"
			}
			if _, seen := byID[e.RequestID]; !seen {
				order = append(order, e.RequestID)
			}
			cp := e
			byID[e.RequestID] = &cp
		}
		// Keep only the newest maxEntries by time.
		if len(order) > maxEntries {
			order = order[len(order)-maxEntries:]
		}
		store.Lock()
		for _, id := range order {
			e := byID[id]
			if e == nil {
				continue
			}
			store.order = append(store.order, e)
			store.byID[id] = e
		}
		store.Unlock()
	})
}

var effortSuffixRe = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// preferEffortModel keeps the variant that carries a thinking suffix like
// "(high)"/"(low)" when both names share the same base model.
func preferEffortModel(cur, next string) string {
	if next == "" {
		return cur
	}
	if cur == "" {
		return next
	}
	if effortSuffixRe.ReplaceAllString(cur, "") == effortSuffixRe.ReplaceAllString(next, "") {
		if strings.Contains(cur, "(") {
			return cur
		}
		return next
	}
	return next
}

func findLatestByTraceLocked(traceID string) *logEntry {
	for i := len(store.order) - 1; i >= 0; i-- {
		if store.order[i].TraceID == traceID {
			return store.order[i]
		}
	}
	return nil
}

func upsertEntry(id string) *logEntry {
	cfg := configOrDefault()
	e, ok := store.byID[id]
	if !ok {
		e = &logEntry{RequestID: id}
		store.byID[id] = e
		store.order = append(store.order, e)
	}
	for len(store.order) > cfg.MaxEntries && cfg.MaxEntries > 0 {
		oldest := store.order[0]
		store.order = store.order[1:]
		delete(store.byID, oldest.RequestID)
	}
	return e
}

func entriesSnapshot() []*logEntry {
	store.Lock()
	defer store.Unlock()
	// Sweep rows stuck in "running" for >5min: the terminal event was lost.
	staleBefore := time.Now().Unix() - 300
	out := make([]*logEntry, 0, len(store.order))
	for i := len(store.order) - 1; i >= 0; i-- {
		e := store.order[i]
		if e.Outcome == "running" && e.Time > 0 && e.Time < staleBefore {
			e.Outcome = "stale"
			e.Running = false
			e.Error = "no terminal event recorded (stale)"
		}
		out = append(out, e)
	}
	return out
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      registrationMetadata   `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationMetadata struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	Author           string `json:"author"`
	GitHubRepository string `json:"GitHubRepository"`
}

type registrationCapability struct {
	UsagePlugin            bool `json:"usage_plugin"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	RequestInterceptor     bool `json:"request_interceptor"`
	ManagementAPI          bool `json:"management_api"`
}

// requestInterceptWire mirrors pluginapi.RequestInterceptRequest on the wire.
type requestInterceptWire struct {
	RequestID      string         `json:"RequestID"`
	TraceID        string         `json:"TraceID"`
	SourceFormat   string         `json:"SourceFormat"`
	ToFormat       string         `json:"ToFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	Stream         bool           `json:"Stream"`
	Body           []byte         `json:"Body"` // base64 on the wire
	Metadata       map[string]any `json:"Metadata"`
}

const maxPromptBytes = 12288

// extractPrompt pulls the last user-message text out of a client request
// body. Handles chat-completions (messages), responses API (input) and
// Gemini-style (contents[].parts[].text) payloads.
func extractPrompt(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if msgs, ok := m["messages"].([]any); ok {
		for i := len(msgs) - 1; i >= 0; i-- {
			msg, ok := msgs[i].(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "" || role == "user" {
				if t := contentText(msg["content"]); t != "" {
					return t
				}
			}
		}
	}
	switch inp := m["input"].(type) {
	case string:
		if inp != "" {
			return inp
		}
	case []any:
		for i := len(inp) - 1; i >= 0; i-- {
			if item, ok := inp[i].(map[string]any); ok {
				if t := contentText(item["content"]); t != "" {
					return t
				}
			}
		}
	}
	if cs, ok := m["contents"].([]any); ok {
		for i := len(cs) - 1; i >= 0; i-- {
			if c, ok := cs[i].(map[string]any); ok {
				if t := contentText(c["parts"]); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["text"].(string); t != "" {
					if sb.Len() > 0 {
						sb.WriteString(" ")
					}
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// Wire mirrors of pluginapi types (PascalCase keys on the wire).

type usageDetailWire struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
	TokenBreakdown      struct {
		Input struct {
			TotalTokens      int64 `json:"total_tokens"`
			UncachedTokens   int64 `json:"uncached_tokens"`
			CacheReadTokens  int64 `json:"cache_read_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"input"`
	} `json:"TokenBreakdown"`
}

// inputTotal resolves the true total input tokens. Some providers count
// cache-read inside InputTokens (Gemini-style) while others keep buckets
// exclusive (Claude-style) — the canonical breakdown is authoritative.
func (d usageDetailWire) inputTotal() int64 {
	if t := d.TokenBreakdown.Input.TotalTokens; t > 0 {
		return t
	}
	// Fallback: if uncached-only style, input total is the sum of buckets.
	if d.InputTokens > 0 && d.CacheReadTokens > d.InputTokens {
		return d.InputTokens + d.CacheReadTokens + d.CacheCreationTokens
	}
	return d.InputTokens
}

type usageFailureWire struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageRecordWire struct {
	RequestID     string           `json:"RequestID"`
	TraceID       string           `json:"TraceID"`
	Provider      string           `json:"Provider"`
	BaseURL       string           `json:"BaseURL"`
	ExecutorType  string           `json:"ExecutorType"`
	Model         string           `json:"Model"`
	Alias         string           `json:"Alias"`
	APIKey        string           `json:"APIKey"`
	SessionID     string           `json:"SessionID"`
	ParentSession string           `json:"ParentSessionID"`
	AuthID        string           `json:"AuthID"`
	AuthIndex     string           `json:"AuthIndex"`
	AuthType      string           `json:"AuthType"`
	Source        string           `json:"Source"`
	ReasoningEff  string           `json:"ReasoningEffort"`
	ResponseModel string           `json:"ResponseModel"`
	Generate      bool             `json:"Generate"`
	Stream        bool             `json:"Stream"`
	RequestedAt   time.Time        `json:"RequestedAt"`
	Latency       time.Duration    `json:"Latency"`
	TTFT          time.Duration    `json:"TTFT"`
	Failed        bool             `json:"Failed"`
	Failure       usageFailureWire `json:"Failure"`
	Detail        usageDetailWire  `json:"Detail"`
}

type requestCompletionWire struct {
	RequestID      string         `json:"RequestID"`
	TraceID        string         `json:"TraceID"`
	SourceFormat   string         `json:"SourceFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	Stream         bool           `json:"Stream"`
	Outcome        string         `json:"Outcome"`
	StatusCode     int            `json:"StatusCode"`
	Error          string         `json:"Error"`
	StartedAt      time.Time      `json:"StartedAt"`
	CompletedAt    time.Time      `json:"CompletedAt"`
	Metadata       map[string]any `json:"Metadata"`
}

type managementRequestWire struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers"`
	Query   map[string][]string `json:"Query"`
	Body    []byte              `json:"Body"`
}

type managementResponseWire struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodRequestComplete:
		return handleComplete(request)
	case pluginabi.MethodRequestInterceptBefore:
		return handleInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return handleInterceptAfter(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := pluginConfig{MaxEntries: 500}
	if len(req.ConfigYAML) > 0 {
		if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
			return errDecode
		}
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 500
	}
	cfg.PersistFile = strings.TrimSpace(cfg.PersistFile)
	currentConfig.Store(cfg)
	if cfg.PersistFile != "" {
		if dir := filepath.Dir(cfg.PersistFile); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Printf("request-logs: cannot create persist dir: %v", err)
			}
		}
	}
	persistLoadOnce()
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: registrationMetadata{
			Name:             "request-logs",
			Version:          "1.0.0",
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
		},
		Capabilities: registrationCapability{
			UsagePlugin:            true,
			RequestLifecyclePlugin: true,
			RequestInterceptor:     true,
			ManagementAPI:          true,
		},
	}
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []map[string]any{
			{"Method": http.MethodGet, "Path": "/request-logs/entries"},
			{"Method": http.MethodPost, "Path": "/request-logs/clear"},
		},
		"resources": []map[string]any{
			{"Path": "/logs", "Menu": "Request Logs", "Description": "Request log table (OmniRoute-style)"},
		},
	}
}

// handleInterceptBefore creates the "running" parent row when a request is
// admitted. The returned empty RequestInterceptResponse never terminates.
func handleInterceptBefore(raw []byte) ([]byte, error) {
	var req requestInterceptWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.RequestID != "" {
		store.Lock()
		e := upsertEntry(req.RequestID)
		e.isParent = true
		e.TraceID = req.TraceID
		e.Time = time.Now().Unix()
		e.Model = req.Model
		e.Requested = req.RequestedModel
		e.Stream = req.Stream
		if p := extractPrompt(req.Body); p != "" {
			e.Prompt = truncate(p, maxPromptBytes)
		}
		e.Outcome = "running"
		e.Running = true
		store.Unlock()
		persistAppend(e)
	}
	return okEnvelope(map[string]any{})
}

// handleInterceptAfter fills the selected upstream format/model per attempt.
func handleInterceptAfter(raw []byte) ([]byte, error) {
	var req requestInterceptWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.RequestID == "" {
		return okEnvelope(map[string]any{})
	}
	store.Lock()
	e, ok := store.byID[req.RequestID]
	if !ok {
		e = upsertEntry(req.RequestID)
		e.isParent = true
		e.Outcome = "running"
		e.Running = true
		if e.Time == 0 {
			e.Time = time.Now().Unix()
		}
	}
	if e.TraceID == "" {
		e.TraceID = req.TraceID
	}
	if req.ToFormat != "" {
		e.Provider = req.ToFormat
	}
	if req.Model != "" {
		e.Model = preferEffortModel(e.Model, req.Model)
	}
	if e.Requested == "" {
		e.Requested = req.RequestedModel
	}
	e.Stream = e.Stream || req.Stream
	store.Unlock()
	persistAppend(e)
	return okEnvelope(map[string]any{})
}

func handleUsage(raw []byte) ([]byte, error) {
	var rec usageRecordWire
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	if rec.RequestID == "" {
		return okEnvelope(map[string]any{"skipped": true})
	}
	store.Lock()
	e, ok := store.byID[rec.RequestID]
	parentRow := false
	if !ok && rec.TraceID != "" {
		// Fold per-attempt usage into the parent request row so the table
		// stays one-row-per-request; the chain lives in e.Attempts.
		if existing := findLatestByTraceLocked(rec.TraceID); existing != nil && existing.isParent {
			e = existing
			parentRow = true
		}
	}
	if e == nil {
		e = upsertEntry(rec.RequestID)
	}
	e.hasUsage = true
	e.TraceID = rec.TraceID
	if !rec.RequestedAt.IsZero() {
		e.Time = rec.RequestedAt.Unix()
	}
	if e.Time == 0 {
		e.Time = time.Now().Unix()
	}
	account := rec.AuthID
	if strings.Contains(rec.Source, "@") {
		account = rec.Source
	}
	av := attemptView{
		InputTotal: rec.Detail.inputTotal(),
		Provider:   rec.Provider,
		Model:      rec.Model,
		Account:    maskAccount(account),
		Status:     rec.Failure.StatusCode,
		Failed:     rec.Failed,
		Error:      truncate(rec.Failure.Body, 200),
		TI:         rec.Detail.InputTokens,
		TO:         rec.Detail.OutputTokens,
		Reasoning:  rec.Detail.ReasoningTokens,
		CacheRead:  rec.Detail.CacheReadTokens,
		CacheWrite: rec.Detail.CacheCreationTokens,
		TTFTms:     rec.TTFT.Milliseconds(),
		DurationMs: rec.Latency.Milliseconds(),
	}
	if !av.Failed && av.Status == 0 {
		av.Status = 200
	}
	if rec.Latency > 0 && rec.Detail.OutputTokens > 0 {
		av.TPS = int64(float64(rec.Detail.OutputTokens) / rec.Latency.Seconds())
	}
	e.Attempts = append(e.Attempts, av)
	e.Provider = rec.Provider
	e.Executor = rec.ExecutorType
	e.Model = preferEffortModel(e.Model, rec.Model)
	e.ResponseModel = rec.ResponseModel
	if rec.Alias != "" {
		e.Requested = rec.Alias
	}
	e.Account = av.Account
	e.AuthType = rec.AuthType
	e.APIKey = maskAPIKey(rec.APIKey)
	e.Stream = rec.Stream
	e.Session = rec.SessionID
	e.ParentSession = rec.ParentSession
	e.Source = rec.Source
	e.ReasoningLevel = rec.ReasoningEff
	e.BaseURL = rec.BaseURL
	e.TokensIn = rec.Detail.InputTokens
	e.TokensOut = rec.Detail.OutputTokens
	e.Reasoning = rec.Detail.ReasoningTokens
	e.CacheRead = rec.Detail.CacheReadTokens
	e.CacheWrite = rec.Detail.CacheCreationTokens
	e.InputTotal = rec.Detail.inputTotal()
	e.TokensTotal = rec.Detail.TotalTokens
	e.DurationMs = rec.Latency.Milliseconds()
	e.TTFTms = rec.TTFT.Milliseconds()
	e.TPS = av.TPS
	if rec.Failed {
		e.Status = rec.Failure.StatusCode
		e.Error = truncate(rec.Failure.Body, 400)
	}
	if !parentRow {
		// Standalone rows (no intercept) resolve on the usage outcome.
		if rec.Failed {
			e.Outcome = "failed"
		} else {
			e.Outcome = "succeeded"
			if e.Status == 0 {
				e.Status = 200
			}
		}
	}
	store.Unlock()
	persistAppend(e)
	return okEnvelope(map[string]any{"recorded": true})
}

func handleComplete(raw []byte) ([]byte, error) {
	var c requestCompletionWire
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.RequestID == "" {
		return okEnvelope(map[string]any{"skipped": true})
	}
	store.Lock()
	e, ok := store.byID[c.RequestID]
	if !ok && c.TraceID != "" {
		// usage.handle and request.complete use different request IDs for the
		// same attempt; merge the terminal event into the newest attempt row.
		e = findLatestByTraceLocked(c.TraceID)
	}
	if e == nil {
		if c.Model == "" && (c.Outcome == "" || c.Outcome == "succeeded") && c.Error == "" {
			// Empty terminal event (e.g. non-model request); not worth a row.
			store.Unlock()
			return okEnvelope(map[string]any{"skipped": true})
		}
		e = upsertEntry(c.RequestID)
	}
	if e.TraceID == "" {
		e.TraceID = c.TraceID
	}
	if e.Time == 0 && !c.StartedAt.IsZero() {
		e.Time = c.StartedAt.Unix()
	}
	if e.Model == "" {
		e.Model = c.Model
	}
	if e.Requested == "" {
		e.Requested = c.RequestedModel
	}
	e.Stream = c.Stream || e.Stream
	e.Outcome = c.Outcome
	e.Running = false
	if c.StatusCode != 0 {
		e.Status = c.StatusCode
	} else if e.Status == 0 && c.Outcome == "succeeded" {
		e.Status = 200
	}
	if c.Error != "" {
		e.Error = truncate(c.Error, 400)
	}
	if e.DurationMs == 0 && !c.StartedAt.IsZero() && !c.CompletedAt.IsZero() {
		e.DurationMs = c.CompletedAt.Sub(c.StartedAt).Milliseconds()
	}
	store.Unlock()
	persistAppend(e)
	return okEnvelope(map[string]any{"recorded": true})
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := req.Path
	switch {
	case strings.HasPrefix(path, "/v0/resource/plugins/request-logs/"):
		return okEnvelope(managementResponseWire{
			StatusCode: http.StatusOK,
			Headers:    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
			Body:       []byte(pageHTML),
		})
	case strings.HasSuffix(path, "/request-logs/entries"):
		return entriesJSON()
	case strings.HasSuffix(path, "/request-logs/clear"):
		store.Lock()
		store.order = nil
		store.byID = make(map[string]*logEntry)
		store.Unlock()
		persistTruncate()
		return okEnvelope(managementResponseWire{
			StatusCode: http.StatusOK,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"ok":true}`),
		})
	default:
		return okEnvelope(managementResponseWire{
			StatusCode: http.StatusNotFound,
			Headers:    map[string][]string{"Content-Type": {"application/json"}},
			Body:       []byte(`{"error":"not found"}`),
		})
	}
}

func entriesJSON() ([]byte, error) {
	entries := entriesSnapshot()
	stats := map[string]int{"total": len(entries)}
	providers := map[string]struct{}{}
	models := map[string]struct{}{}
	runningByProvider := map[string]int{}
	accounts := map[string]struct{}{}
	apiKeys := map[string]struct{}{}
	for _, e := range entries {
		switch {
		case e.Running || e.Outcome == "running":
			stats["running"]++
			if e.Provider != "" {
				runningByProvider[e.Provider]++
			}
		case e.Outcome == "rejected" || e.Outcome == "canceled" || e.Outcome == "stale":
			stats["rejected"]++
		case e.Status >= 400 || e.Failed():
			stats["error"]++
		default:
			stats["success"]++
		}
		if e.Provider != "" {
			providers[e.Provider] = struct{}{}
		}
		if e.Model != "" {
			models[e.Model] = struct{}{}
		}
		if e.Account != "" {
			accounts[e.Account] = struct{}{}
		}
		if e.APIKey != "" {
			apiKeys[e.APIKey] = struct{}{}
		}
	}
	providerList := keysOf(providers)
	modelList := keysOf(models)
	payload, err := json.Marshal(map[string]any{
		"entries":             entries,
		"stats":               stats,
		"providers":           providerList,
		"models":              modelList,
		"accounts":            keysOf(accounts),
		"api_keys":            keysOf(apiKeys),
		"running_by_provider": runningByProvider,
		"updated":             time.Now().Unix(),
	})
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponseWire{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       payload,
	})
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (e *logEntry) Failed() bool { return e.Outcome == "failed" }

func maskAccount(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if at := strings.Index(id, "@"); at > 0 {
		local := id[:at]
		if len(local) > 3 {
			local = local[:3] + "***"
		}
		return local + "@***"
	}
	if len(id) > 6 {
		return id[:3] + "***" + id[len(id)-2:]
	}
	return id
}

func maskAPIKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) > 8 {
		return k[:3] + "***" + k[len(k)-4:]
	}
	return "***"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(envelope{OK: true, Result: raw})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func errorEnvelope(code, message string) []byte {
	out, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return out
}

func writeResponse(response *C.cliproxy_buffer, payload []byte) {
	if response == nil || len(payload) == 0 {
		return
	}
	size := C.size_t(len(payload))
	mem := C.malloc(size)
	if mem == nil {
		return
	}
	copy((*[1 << 30]byte)(mem)[:len(payload)], payload)
	response.ptr = mem
	response.len = size
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Request Logs</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--bg:#0d0f14;--panel:#14171f;--border:#232733;--fg:#dbe1ea;--dim:#8b93a3;--ok:#34d399;--err:#f87171;--warn:#fbbf24;--accent:#60a5fa}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:13px/1.5 -apple-system,"SF Pro","Segoe UI",Roboto,sans-serif}
header{display:flex;align-items:center;gap:12px;padding:14px 18px;border-bottom:1px solid var(--border);background:var(--panel);flex-wrap:wrap}
h1{font-size:16px;margin:0;font-weight:600}
.dot{width:8px;height:8px;border-radius:50%;background:var(--err);display:inline-block}
.dot.on{background:var(--ok);box-shadow:0 0 6px var(--ok)}
.chips{display:flex;gap:8px;flex-wrap:wrap}
.chip{padding:3px 10px;border:1px solid var(--border);border-radius:999px;color:var(--dim);background:#0f1219;cursor:default}
.chip b{color:var(--fg)}
.chip.ok b{color:var(--ok)} .chip.err b{color:var(--err)}
.controls{display:flex;gap:8px;align-items:center;margin-left:auto;flex-wrap:wrap}
input,select,button{background:#0f1219;border:1px solid var(--border);color:var(--fg);border-radius:6px;padding:6px 10px;font-size:12px}
button{cursor:pointer} button:hover{border-color:var(--accent)}
button.danger{color:var(--err)}
table{width:100%;border-collapse:collapse;font-size:12px}
thead th{position:sticky;top:0;background:var(--panel);color:var(--dim);text-align:left;padding:8px 10px;border-bottom:1px solid var(--border);font-weight:600;text-transform:uppercase;font-size:10px;letter-spacing:.4px;white-space:nowrap}
tbody td{padding:6px 10px;border-bottom:1px solid #1a1e28;white-space:nowrap;max-width:260px;overflow:hidden;text-overflow:ellipsis}
tbody tr:hover{background:#161a24}
.st{padding:1px 7px;border-radius:5px;font-weight:700;font-size:11px}
.s2{background:#0d3325;color:var(--ok)} .s4{background:#3b2410;color:var(--warn)} .s5{background:#3b1013;color:var(--err)} .sX{background:#2a2f3d;color:var(--dim)}
.srun{background:#12283d;color:var(--accent);display:inline-flex;align-items:center;gap:5px}
.spin{width:9px;height:9px;border:2px solid #2a4a6b;border-top-color:var(--accent);border-radius:50%;display:inline-block;animation:sp .7s linear infinite}
@keyframes sp{to{transform:rotate(360deg)}}
tr.run td{background:#0f1826}
.chip.run b{color:var(--accent)}
#runProv{display:flex;gap:6px}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.dim{color:var(--dim)}
.prov{padding:1px 7px;border-radius:5px;background:#1d2433;color:#9db4e8;font-weight:600}
.provf{background:#3b1013;color:#f08a95;text-decoration:line-through}
.arr{color:#5a6478;font-size:10px}
tr.open td{border-bottom-color:transparent}
td.detail{background:#10141c;padding:10px 14px}
.att{font-size:11px;color:#9aa4b5;margin:4px 0}
.att b{color:#c8d3e0}
tr.err td{background:#1c1216}
.detail td{background:#10141c;color:var(--dim);white-space:pre-wrap;word-break:break-all}
#auth{position:fixed;inset:0;background:rgba(0,0,0,.7);display:flex;align-items:center;justify-content:center;z-index:9}
#auth .box{background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:24px;width:340px}
#colPanel{display:none;background:var(--panel);border-bottom:1px solid var(--border);padding:8px 14px}
#colPanel.open{display:block}
#colList{display:flex;flex-wrap:wrap;gap:10px}
#colList label{display:flex;gap:4px;align-items:center;color:#9aa4b5;font-size:11px;cursor:pointer;text-transform:uppercase}
#auth input{width:100%;margin:10px 0}
.empty{padding:40px;text-align:center;color:var(--dim)}
</style>
<style id="colstyle"></style>
</head>
<body>
<div id="auth"><div class="box"><b>Management key required</b><div class="dim" style="margin:6px 0">Enter the same secret used for <code>/v0/management</code></div><input id="key" type="password" placeholder="Management secret key"><button onclick="saveKey()">Save</button><div id="authmsg" class="dim"></div></div></div>
<header>
<span class="dot" id="livedot"></span><h1>Request Logs</h1>
<div class="chips">
<span class="chip">Total <b id="cTotal">0</b></span>
<span class="chip run">Running <b id="cRun">0</b></span>
<span class="chip ok">Success <b id="cOk">0</b></span>
<span class="chip err">Error <b id="cErr">0</b></span>
<span class="chip">Shown <b id="cShown">0</b></span>
<span id="runProv"></span>
</div>
<div class="controls">
<input id="q" placeholder="Search model, provider, account..." oninput="render()" size="26">
<select id="fStatus" onchange="render()"><option value="">All status</option><option value="run">Running</option><option value="2">2xx</option><option value="4">4xx</option><option value="5">5xx</option><option value="rej">Rejected/Canceled</option></select>
<select id="fProv" onchange="render()"><option value="">All providers</option></select>
<select id="fModel" onchange="render()"><option value="">All models</option></select>
<select id="fAcct" onchange="render()"><option value="">All accounts</option></select>
<select id="fKey" onchange="render()"><option value="">All API keys</option></select>
<select id="fSort" onchange="render()"><option value="new">Newest</option><option value="old">Oldest</option><option value="dur">Duration ↓</option><option value="tok">Tokens ↓</option><option value="ttft">TTFT ↓</option></select>
<button onclick="toggleLive()" id="liveBtn">⏸ Pause</button>
<button onclick="toggleColPanel()" title="Columns">☰ Cols</button>
<button onclick="refreshNow()" title="Refresh">⟳</button>
<button class="danger" onclick="clearLogs()">Clean history</button>
</div>
</header>
<div id="colPanel"><div id="colList"></div></div>
<table><thead><tr>
<th class="c-status">Status</th><th class="c-model">Model</th><th class="c-requested">Requested</th><th class="c-provider">Provider</th><th class="c-account">Account</th><th class="c-apikey">API Key</th><th class="c-tokens">Tokens</th><th class="c-tps">TPS</th><th class="c-ttft">TTFT</th><th class="c-duration">Duration</th><th class="c-stream">Stream</th><th class="c-time">Time</th><th class="c-trace">Trace</th><th class="c-prompt">Prompt</th>
</tr></thead><tbody id="tb"></tbody></table>
<div class="empty" id="empty" style="display:none">No requests logged yet.</div>
<script>
let rows=[], live=true, openId=null, KEY=localStorage.getItem('cpa-rl-key')||'';
const COLS=[['status','Status'],['model','Model'],['requested','Requested'],['provider','Provider'],['account','Account'],['apikey','API Key'],['tokens','Tokens'],['tps','TPS'],['ttft','TTFT'],['duration','Duration'],['stream','Stream'],['time','Time'],['trace','Trace'],['prompt','Prompt']];
let hiddenCols=new Set(JSON.parse(localStorage.getItem('cpa-rl-cols')||'[]'));
function applyCols(){document.getElementById('colstyle').textContent=[...hiddenCols].map(k=>'.c-'+k+'{display:none}').join('');}
function toggleColPanel(){const p=document.getElementById('colPanel');p.classList.toggle('open');if(p.classList.contains('open'))renderColPanel();}
function renderColPanel(){
 const l=document.getElementById('colList');l.innerHTML='';
 COLS.forEach(([k,lab])=>{const lb=document.createElement('label');const cb=document.createElement('input');cb.type='checkbox';cb.checked=!hiddenCols.has(k);cb.onchange=()=>{if(cb.checked)hiddenCols.delete(k);else hiddenCols.add(k);localStorage.setItem('cpa-rl-cols',JSON.stringify([...hiddenCols]));applyCols()};lb.appendChild(cb);lb.appendChild(document.createTextNode(lab));l.appendChild(lb)});
}
applyCols();
const DATA='/v0/management/request-logs/entries';
const CLEAR='/v0/management/request-logs/clear';
if(KEY)document.getElementById('auth').style.display='none';
function esc(s){return String(s==null?'':s).replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
function fmtTime(t){const d=new Date(t*1000);return d.toLocaleTimeString('en-GB',{hour12:false})}
function fmtMs(ms){if(!ms)return'—';return ms>=1000?(ms/1000).toFixed(2)+'s':ms+'ms'}
function stClass(s){if(s>=500)return's5';if(s>=400)return's4';if(s>=200)return's2';return'sX'}
function stLabel(e){
 if(e.outcome==='running')return'<span class="st srun"><span class="spin"></span>RUN</span>';
 if(e.outcome==='rejected')return'<span class="st sX">REJ</span>';
 if(e.outcome==='canceled')return'<span class="st sX">CAN</span>';
 if(e.outcome==='stale')return'<span class="st s5">STALE</span>';
 return '<span class="st '+stClass(e.status)+'">'+(e.status||'—')+'</span>';
}
function fill(sel,list){const el=document.getElementById(sel);const cur=el.value;while(el.options.length>1)el.remove(1);list.forEach(v=>{const o=document.createElement('option');o.value=v;o.textContent=v;el.appendChild(o)});el.value=cur}
async function fetchData(){
 if(!KEY)return; // never probe the endpoint without a key: the host bans repeated failures
 try{
  const r=await fetch(DATA,{headers:{'Authorization':'Bearer '+KEY,'X-Management-Key':KEY}});
  if(r.status===401||r.status===403){live=false;document.getElementById('liveBtn').textContent='▶ Resume';document.getElementById('livedot').className='dot';document.getElementById('auth').style.display='flex';document.getElementById('authmsg').textContent='Key rejected — enter a valid management key';return}
  document.getElementById('auth').style.display='none';
  const d=await r.json();rows=d.entries||[];
  document.getElementById('cTotal').textContent=d.stats.total||0;
  document.getElementById('cRun').textContent=d.stats.running||0;
  document.getElementById('cOk').textContent=d.stats.success||0;
  document.getElementById('cErr').textContent=(d.stats.error||0)+(d.stats.rejected||0);
  const rp=document.getElementById('runProv');rp.innerHTML='';
  for(const[p,n]of Object.entries(d.running_by_provider||{})){const c=document.createElement('span');c.className='chip run';c.innerHTML='⟳ '+esc(p)+' <b>'+n+'</b>';rp.appendChild(c)}
  fill('fProv',d.providers||[]);fill('fModel',d.models||[]);fill('fAcct',d.accounts||[]);fill('fKey',d.api_keys||[]);
  render();
 }catch(e){/*keep old rows*/}
}
function saveKey(){KEY=document.getElementById('key').value.trim();localStorage.setItem('cpa-rl-key',KEY);live=true;document.getElementById('liveBtn').textContent='⏸ Pause';document.getElementById('livedot').className='dot on';fetchData();}
function render(){
 const q=document.getElementById('q').value.toLowerCase();
 const fs=document.getElementById('fStatus').value,fp=document.getElementById('fProv').value,fm=document.getElementById('fModel').value;
 const fa=document.getElementById('fAcct').value,fk=document.getElementById('fKey').value;
 const sort=document.getElementById('fSort').value;
 const view=sort==='new'?rows:[...rows].sort((a,b)=>sort==='old'?a.time-b.time:sort==='dur'?(b.duration_ms||0)-(a.duration_ms||0):sort==='tok'?(b.total||0)-(a.total||0):(b.ttft_ms||0)-(a.ttft_ms||0));
 const tb=document.getElementById('tb');tb.innerHTML='';let shown=0;
 for(const e of view){
  const running=e.outcome==='running';
  const cls=running?'run':e.status>=500?'5':e.status>=400?'4':e.status>=200?'2':'rej';
  if(fs&&cls!==fs)continue;
  if(fs==='rej'&&!(e.outcome==='rejected'||e.outcome==='canceled'||e.outcome==='stale'))continue;
  if(fp&&e.provider!==fp&&!(e.attempts||[]).some(a=>a.provider===fp))continue;
  if(fm&&e.model!==fm&&!(e.attempts||[]).some(a=>a.model===fm))continue;
  if(fa&&e.account!==fa&&!(e.attempts||[]).some(a=>a.account===fa))continue;
  if(fk&&e.api_key!==fk)continue;
  if(q&&!(e.model+' '+(e.requested||'')+' '+e.provider+' '+e.account+' '+(e.api_key||'')+' '+(e.trace_id||'')+' '+(e.prompt||'')).toLowerCase().includes(q))continue;
  shown++;
  const tr=document.createElement('tr');if(cls==='4'||cls==='5'||e.outcome==='failed')tr.className='err';if(running)tr.className='run';
  const inTotal=e.input_total||e.ti;
  const cpct=inTotal>0&&e.cache_read>0?Math.min(100,e.cache_read*100/inTotal):0;
  const toks='TI:'+e.ti+' TO:'+e.to+(e.reasoning?' R:'+e.reasoning:'')+(cpct?' <span style="color:var(--warn)">⚡'+cpct.toFixed(1)+'%</span>':'');
  const dur=running?Math.max(0,Math.floor(Date.now()/1000)-e.time)*1000:e.duration_ms;
  tr.innerHTML='<td class="c-status">'+stLabel(e)+'</td>'+
   '<td class="mono c-model" style="color:#f0a8b8">'+esc(e.model||e.requested||'—')+'</td>'+
   '<td class="mono c-requested" style="color:#d9a845">'+esc(e.requested||'—')+'</td>'+
   '<td class="c-provider">'+provChain(e)+'</td>'+
   '<td class="dim c-account">'+esc(e.account||'—')+'</td>'+
   '<td class="dim c-apikey">'+esc(e.api_key||'—')+'</td>'+
   '<td class="dim c-tokens">'+toks+'</td>'+
   '<td class="c-tps">'+(e.tps?e.tps:'—')+'</td>'+
   '<td class="c-ttft">'+fmtMs(e.ttft_ms)+'</td>'+
   '<td class="c-duration">'+fmtMs(dur)+'</td>'+
   '<td class="c-stream">'+(e.stream?'stream':'—')+'</td>'+
   '<td class="dim c-time">'+fmtTime(e.time)+'</td>'+
   '<td class="dim mono c-trace">'+(e.trace_id?esc(e.trace_id.slice(0,8)):'—')+'</td>'+
   '<td class="dim c-prompt" style="max-width:220px">'+esc((e.prompt||'').slice(0,110))+(e.prompt&&e.prompt.length>110?'…':'')+'</td>';
  if(e.id===openId)tr.classList.add('open');
  tr.onclick=()=>{openId=(openId===e.id)?null:e.id;render()};
  tb.appendChild(tr);
  if(e.id===openId)tb.appendChild(detailRow(e));
 }
 document.getElementById('cShown').textContent=shown;
 document.getElementById('empty').style.display=shown?'none':'block';
}
function provChain(e){
 const at=e.attempts||[];
 if(at.length===0)return'<span class="prov">'+esc(e.provider||'—')+'</span>';
 if(at.length===1){const a=at[0];return'<span class="'+(a.failed?'prov provf':'prov')+'">'+esc(a.provider||e.provider||'—')+'</span>'}
 return at.map(a=>'<span class="'+(a.failed?'prov provf':'prov')+'">'+esc(a.provider||'?')+'</span>').join('<span class="arr">→</span>');
}
function detailRow(e){
 const d=document.createElement('tr');d.className='detail';
 let html='';
 const at=e.attempts||[];
 if(at.length>0){
  html+='<div class="att"><b>Route:</b> '+(e.requested||'—')+' → '+(at.length>1?'<span style="color:var(--warn)">fell back ×'+(at.length-1)+'</span>':'direct')+'</div>';
  at.forEach((a,i)=>{
   const aInTotal=a.input_total||a.ti;
   const cpct=aInTotal>0&&a.cache_read>0?Math.min(100,a.cache_read*100/aInTotal):0;
   const tag=i===0&&at.length>1?'primary':(i>0?'fallback #'+i:'only');
   const reason=a.failed?(' <span style="color:var(--err)">— reason: '+(a.status||'')+' '+esc((a.error||'').slice(0,140))+'</span>'):'';
   html+='<div class="att">'+tag+' · <b>'+esc(a.provider||'?')+'</b> '+(a.model?'('+esc(a.model)+') ':'')+(a.failed?'<span style="color:var(--err)">✗ '+(a.status||'')+'</span>':'<span style="color:var(--ok)">✓ '+a.status+'</span>')+reason+' '+esc(a.account||'')+' TI:'+a.ti+' TO:'+a.to+(a.reasoning?' R:'+a.reasoning:'')+(cpct?' ⚡'+cpct.toFixed(1)+'%':'')+' '+fmtMs(a.duration_ms)+(a.ttft_ms?' ttft '+fmtMs(a.ttft_ms):'')+'</div>';
  });
 }
 if(e.prompt){
  html+='<div class="att"><b>Prompt:</b></div><pre class="mono" style="margin:4px 0;color:#c8d3e0;max-height:240px;overflow:auto;white-space:pre-wrap">'+esc(e.prompt)+'</pre>';
 }
 const info={id:e.id,trace:e.trace_id,outcome:e.outcome,response_model:e.response_model,executor:e.executor,auth_type:e.auth_type,session:e.session,parent:e.parent_session,source:e.source,reasoning:e.reasoning_level,base_url:e.base_url,total_tokens:e.total,cache_read:e.cache_read,cache_write:e.cache_write};
 let lines=Object.entries(info).filter(([,v])=>v!==''&&v!=null&&v!==0).map(([k,v])=>k+': '+v).join('\n');
 html+='<pre class="mono" style="margin:6px 0;color:#9aa4b5">'+esc(lines)+(e.error?esc('\n\nERROR:\n'+e.error):'')+'</pre>';
 d.innerHTML='<td colspan="14" class="detail">'+html+'</td>';
 return d;
}
function toggleLive(){live=!live;document.getElementById('liveBtn').textContent=live?'⏸ Pause':'▶ Resume';document.getElementById('livedot').className='dot'+(live?' on':'')}
function refreshNow(){fetchData()}
async function clearLogs(){if(!confirm('Clear all request logs?'))return;await fetch(CLEAR,{method:'POST',headers:{'Authorization':'Bearer '+KEY,'X-Management-Key':KEY}});fetchData()}
document.getElementById('livedot').className='dot on';
setInterval(()=>{if(live)fetchData()},2000);
fetchData();
</script>
</body>
</html>`
