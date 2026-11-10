package helps

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestApplyDevinTextReplacementsRewritesBuiltInFingerprints(t *testing.T) {
	rules := DevinReplacements(nil)
	if len(rules) == 0 {
		t.Fatal("built-in replacements must not be empty")
	}
	for _, rule := range rules {
		got := ApplyDevinTextReplacements(rule.From, rules)
		if strings.Contains(got, rule.From) {
			t.Errorf("rule %q was not rewritten: %q", rule.From, got)
		}
	}
}

func TestApplyDevinTextReplacementsSkipsEmptyFrom(t *testing.T) {
	rules := []config.DevinReplacement{{From: "", To: "boom"}}
	got := ApplyDevinTextReplacements("hello world", rules)
	if got != "hello world" {
		t.Fatalf("empty From must not mutate text, got %q", got)
	}
}

func TestApplyDevinTextReplacementsReplacesEveryOccurrence(t *testing.T) {
	rules := []config.DevinReplacement{{From: "blast radius", To: "impact radius"}}
	got := ApplyDevinTextReplacements("blast radius and blast radius", rules)
	if got != "impact radius and impact radius" {
		t.Fatalf("got %q", got)
	}
}

func TestDevinReplacementsMergesConfiguredRules(t *testing.T) {
	cfg := &config.Config{Devin: config.DevinConfig{Replacements: []config.DevinReplacement{{From: "custom marker", To: "safe marker"}}}}
	rules := DevinReplacements(cfg)
	found := false
	for _, rule := range rules {
		if rule.From == "custom marker" {
			found = true
		}
	}
	if !found {
		t.Fatal("configured rule missing from merged list")
	}
}

func TestScrubDevinFingerprintsCoversSystemPromptsAndTools(t *testing.T) {
	systemPrompt := "You are Claude Code, Anthropic's official CLI for Claude.\nDo not use a colon before tool calls."
	prompts := []DevinPrompt{
		{Source: 1, Content: "The system will automatically compress prior messages in your conversation."},
		{Source: 2, Content: "assistant reply", Thinking: "You MUST access memory when the user explicitly asks you to check, recall, or remember."},
	}
	tools := []DevinTool{
		{Name: "Read", Description: "Reads a file from the local filesystem. You can access any file directly by using this tool."},
	}
	sys, gotPrompts, gotTools := ScrubDevinFingerprints(nil, systemPrompt, prompts, tools)
	if strings.Contains(sys, "official CLI for Claude") {
		t.Errorf("system prompt keeps fingerprint: %q", sys)
	}
	if strings.Contains(sys, "Do not use a colon before tool calls") {
		t.Errorf("system prompt keeps colon fingerprint: %q", sys)
	}
	if strings.Contains(gotPrompts[0].Content, "automatically compress prior messages") {
		t.Errorf("prompt keeps fingerprint: %q", gotPrompts[0].Content)
	}
	if gotPrompts[1].Thinking != prompts[1].Thinking {
		t.Errorf("thinking must stay untouched, got %q", gotPrompts[1].Thinking)
	}
	if strings.Contains(gotTools[0].Description, "Reads a file from the local filesystem") {
		t.Errorf("tool description keeps fingerprint: %q", gotTools[0].Description)
	}
}

func TestTrimDevinPayloadForPolicyRetryKeepsSystemAndLastUser(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"system","content":"be helpful"},{"role":"user","content":"first turn"},{"role":"assistant","content":"reply"},{"role":"user","content":"latest turn"}],"max_tokens":100}`)
	got, ok := TrimDevinPayloadForPolicyRetry(payload)
	if !ok {
		t.Fatal("expected trim to succeed")
	}
	root := ojson.NewDoc(got)
	msgs := root.Get("messages").Array()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages kept, got %d (%s)", len(msgs), root.String())
	}
	if msgs[0].Get("role").String() != "system" || msgs[0].Get("content").String() != "be helpful" {
		t.Fatalf("system turn lost: %s", msgs[0].Raw)
	}
	if msgs[1].Get("role").String() != "user" || msgs[1].Get("content").String() != "latest turn" {
		t.Fatalf("last user turn not kept: %s", msgs[1].Raw)
	}
	if root.Get("model").String() != "claude-opus-5" || root.Get("max_tokens").Int() != 100 {
		t.Fatalf("non-message fields must survive, got %s", root.String())
	}
}

func TestTrimDevinPayloadForPolicyRetryRejectsMissingUser(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"messages":[{"role":"system","content":"only system"},{"role":"assistant","content":"reply"}]}`),
		[]byte(`{"prompt":"no messages array"}`),
		[]byte(`not json`),
	}
	for _, payload := range cases {
		if _, ok := TrimDevinPayloadForPolicyRetry(payload); ok {
			t.Fatalf("expected trim to fail for %s", payload)
		}
	}
}
