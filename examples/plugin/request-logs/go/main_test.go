package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// resetStore clears the shared store and persist state so tests do not leak.
func resetStore(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		store.Lock()
		store.order = nil
		store.byID = make(map[string]*logEntry)
		store.Unlock()
		currentConfig.Store(pluginConfig{MaxEntries: 500})
		persistOnce = sync.Once{}
	})
}

func call(t *testing.T, method string, req any) envelope {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := handleMethod(method, raw)
	if err != nil {
		t.Fatalf("%s error: %v", method, err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("%s not ok: %s", method, out)
	}
	return env
}

func TestUsageFoldsIntoParentByTrace(t *testing.T) {
	resetStore(t)
	call(t, "request.intercept_before", map[string]any{
		"RequestID": "p1", "TraceID": "t1", "Model": "m1", "Stream": true,
	})
	call(t, "usage.handle", map[string]any{
		"RequestID": "u1", "TraceID": "t1", "Provider": "openai",
		"Detail": map[string]any{"InputTokens": 10, "OutputTokens": 5, "TotalTokens": 15},
	})
	store.Lock()
	defer store.Unlock()
	if len(store.order) != 1 {
		t.Fatalf("order len = %d, want 1 (usage folded into parent)", len(store.order))
	}
	e := store.order[0]
	if !e.isParent {
		t.Fatalf("entry isParent = false, want true")
	}
	if len(e.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(e.Attempts))
	}
	if e.TraceID != "t1" {
		t.Fatalf("trace = %q, want t1", e.TraceID)
	}
}

func TestCompleteEmptyOutcomePreservesOutcome(t *testing.T) {
	resetStore(t)
	call(t, "request.intercept_before", map[string]any{"RequestID": "p2", "TraceID": "t2"})
	store.Lock()
	store.byID["p2"].Outcome = "succeeded"
	store.Unlock()
	call(t, "request.complete", map[string]any{"RequestID": "p2", "TraceID": "t2", "Outcome": ""})
	store.Lock()
	defer store.Unlock()
	if got := store.byID["p2"].Outcome; got != "succeeded" {
		t.Fatalf("outcome = %q, want preserved succeeded", got)
	}
	if store.byID["p2"].Running {
		t.Fatalf("running = true, want false after complete")
	}
}

func TestRingBufferEvictsOldest(t *testing.T) {
	resetStore(t)
	cfgYAML, _ := yaml.Marshal(map[string]any{"max_entries": 3})
	call(t, "plugin.register", map[string]any{"config_yaml": cfgYAML})
	for i := 0; i < 6; i++ {
		call(t, "request.intercept_before", map[string]any{"RequestID": "r" + string(rune('a'+i))})
	}
	store.Lock()
	defer store.Unlock()
	if len(store.order) != 3 {
		t.Fatalf("order len = %d, want 3", len(store.order))
	}
	if _, ok := store.byID["ra"]; ok {
		t.Fatalf("oldest entry ra still present, want evicted")
	}
	if _, ok := store.byID["rd"]; !ok {
		t.Fatalf("entry rd missing, want kept")
	}
}

func TestPersistLoadTailCapsRead(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.jsonl")
	cfgYAML, _ := yaml.Marshal(map[string]any{"max_entries": 500, "persist_file": path})
	call(t, "plugin.register", map[string]any{"config_yaml": cfgYAML})

	// Write many large entries so the file exceeds maxPersistLoadBytes.
	big := strings.Repeat("x", 64*1024)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 80; i++ {
		e := logEntry{RequestID: "big" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Time: int64(1000 + i), Prompt: big}
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	// Newest entry must survive the tail read.
	last := logEntry{RequestID: "newest", Time: time.Now().Unix(), Outcome: "succeeded", Prompt: big}
	line, _ := json.Marshal(last)
	f.Write(append(line, '\n'))
	f.Close()

	persistOnce = sync.Once{}
	persistLoadOnce()
	store.Lock()
	defer store.Unlock()
	if _, ok := store.byID["newest"]; !ok {
		t.Fatalf("newest entry missing after tail load")
	}
	if len(store.order) > 500 {
		t.Fatalf("order len = %d, want capped", len(store.order))
	}
}

func TestPersistLoadMarksRunningAsStale(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.jsonl")
	cfgYAML, _ := yaml.Marshal(map[string]any{"max_entries": 10, "persist_file": path})
	call(t, "plugin.register", map[string]any{"config_yaml": cfgYAML})

	line, _ := json.Marshal(logEntry{RequestID: "stale1", Time: time.Now().Unix(), Outcome: "running", Running: true})
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	persistOnce = sync.Once{}
	persistLoadOnce()
	store.Lock()
	defer store.Unlock()
	e := store.byID["stale1"]
	if e == nil {
		t.Fatalf("stale1 not restored")
	}
	if e.Outcome != "stale" || e.Running {
		t.Fatalf("outcome = %q running = %v, want stale/false", e.Outcome, e.Running)
	}
}

func TestClearRequiresPost(t *testing.T) {
	resetStore(t)
	call(t, "request.intercept_before", map[string]any{"RequestID": "keep1"})

	// GET must not wipe the buffer.
	call(t, "management.handle", map[string]any{
		"Method": "GET", "Path": "/v0/management/request-logs/clear",
	})
	store.Lock()
	n := len(store.order)
	store.Unlock()
	if n != 1 {
		t.Fatalf("GET clear wiped the buffer (len = %d), want preserved", n)
	}

	// POST clears it.
	call(t, "management.handle", map[string]any{
		"Method": "POST", "Path": "/v0/management/request-logs/clear",
	})
	store.Lock()
	n = len(store.order)
	store.Unlock()
	if n != 0 {
		t.Fatalf("POST clear left %d entries, want 0", n)
	}
}
