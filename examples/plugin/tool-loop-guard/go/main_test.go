package main

import (
	"encoding/json"
	"testing"
)

func skillCall(skill string) map[string]any {
	args, _ := json.Marshal(map[string]string{"skill": skill})
	return map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id":   "call_1",
			"type": "function",
			"function": map[string]any{
				"name":      "Skill",
				"arguments": string(args),
			},
		}},
	}
}

func TestLoopDetected(t *testing.T) {
	payload := map[string]any{"messages": []any{
		skillCall("test-e2e-browser"), skillCall("test-e2e-browser"), skillCall("test-e2e-browser"),
	}}
	if !loopDetected(payload, "Skill", 3) {
		t.Fatal("expected loop detected")
	}
}

func TestNoLoopDifferentSkills(t *testing.T) {
	payload := map[string]any{"messages": []any{
		skillCall("skill-a"), skillCall("skill-b"), skillCall("skill-c"),
	}}
	if loopDetected(payload, "Skill", 3) {
		t.Fatal("different skills must not trigger")
	}
}

func TestNoLoopBelowThreshold(t *testing.T) {
	payload := map[string]any{"messages": []any{skillCall("x"), skillCall("x")}}
	if loopDetected(payload, "Skill", 3) {
		t.Fatal("2 calls below threshold")
	}
}

func TestModelMatch(t *testing.T) {
	if !modelMatches([]string{"grok-4.7*"}, "grok-4.7(high)") {
		t.Fatal("grok-4.7(high) should match grok-4.7*")
	}
	if modelMatches([]string{"grok-4.7*"}, "glm-5.3-flash") {
		t.Fatal("glm must not match")
	}
}
