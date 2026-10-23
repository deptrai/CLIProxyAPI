package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func interceptBody(t *testing.T, body string) string {
	t.Helper()
	raw, errMarshal := json.Marshal(map[string]any{"RequestID": "r1", "Body": []byte(body)})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	out, err := intercept(raw)
	if err != nil {
		t.Fatalf("intercept: %v", err)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("envelope: %v", errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("not ok: %s", out)
	}
	var resp struct{ Body []byte }
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("result: %v", errUnmarshal)
	}
	return string(resp.Body)
}

func TestReservedToolIDsBecomeUniquePortableIDs(t *testing.T) {
	body := `{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"cpa_tid_v1_AAA","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"cpa_tid_v1_AAA","content":"ok"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"cpa_tid_v1_BBB","name":"Read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"cpa_tid_v1_BBB","content":"ok"}]}
	]}`
	got := interceptBody(t, body)
	if strings.Contains(got, "cpa_tid_v1_") {
		t.Fatalf("reserved prefix survived: %s", got)
	}
	var parsed struct {
		Messages []struct {
			Content []struct {
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	a, b := parsed.Messages[0].Content[0].ID, parsed.Messages[2].Content[0].ID
	if a != parsed.Messages[1].Content[0].ToolUseID {
		t.Fatalf("pair 1 diverged: %q vs %q", a, parsed.Messages[1].Content[0].ToolUseID)
	}
	if b != parsed.Messages[3].Content[0].ToolUseID {
		t.Fatalf("pair 2 diverged: %q vs %q", b, parsed.Messages[3].Content[0].ToolUseID)
	}
	if a == b {
		t.Fatalf("distinct ids collapsed to one value: %q", a)
	}
	if !strings.HasPrefix(a, "toolu_") || len(a) != len("toolu_")+24 {
		t.Fatalf("unexpected id shape: %q", a)
	}
}

func TestCleanPayloadIsUntouched(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`
	if got := interceptBody(t, body); got != "" {
		t.Fatalf("expected no rewrite for clean payload, got: %q", got)
	}
}

func TestStableMapping(t *testing.T) {
	body := `{"id":"cpa_tid_v1_AAA"}`
	first := interceptBody(t, body)
	second := interceptBody(t, body)
	if first == "" || first != second {
		t.Fatalf("mapping not stable: %q vs %q", first, second)
	}
}
