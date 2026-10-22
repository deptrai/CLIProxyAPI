package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// seedConfig installs a plugin config for the test and restores the default
// afterward so pick behaviour does not leak between tests.
func seedConfig(t *testing.T, rrProviders map[string]bool) {
	t.Helper()
	currentConfig.Store(pluginConfig{slotTTL: 30 * time.Minute, rrProviders: rrProviders})
	t.Cleanup(func() { currentConfig.Store(pluginConfig{slotTTL: 30 * time.Minute}) })
}

func pick(t *testing.T, candidates string) pluginapiPickResponse {
	t.Helper()
	raw, err := handleMethod("scheduler.pick", []byte(`{"provider":"devin","candidates":`+candidates+`}`))
	if err != nil {
		t.Fatalf("pickAuth error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("pick not ok: %s", raw)
	}
	var resp pluginapiPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v", err)
	}
	return resp
}

type pluginapiPickResponse struct {
	AuthID  string
	Handled bool
	Reject  bool
	// The wire format uses Go field names (no snake_case tags on the struct).
	RejectCode   string
	RejectReason string
}

func usage(t *testing.T, authID string) {
	t.Helper()
	raw, err := handleMethod("usage.handle", []byte(`{"request_id":"r","auth_id":"`+authID+`"}`))
	if err != nil {
		t.Fatalf("usage error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("usage not ok: %s", raw)
	}
}

const twoLimitedAndUnlimited = `[
	{"ID":"eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
	{"ID":"u2","Provider":"devin","Priority":20,"Metadata":{"max_concurrent":3}},
	{"ID":"glm","Provider":"openrouter","Priority":5}
]`

// resetInflight clears all inflight bookkeeping so tests do not leak state.
func resetInflight(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		inflight.Lock()
		inflight.slots = map[string][]time.Time{}
		inflight.lastPicked = map[string]string{}
		inflight.swept = map[string]int{}
		inflight.Unlock()
	})
}

func TestPickRespectsLimitsAndFallsBack(t *testing.T) {
	seedConfig(t, nil)
	defer func() { inflight.slots = map[string][]time.Time{} }()

	// eps limit 1: first pick takes it.
	if r := pick(t, twoLimitedAndUnlimited); r.AuthID != "eps" || !r.Handled {
		t.Fatalf("pick1 = %+v, want eps", r)
	}
	// eps busy -> u2 (limit 3) three times.
	for i := 0; i < 3; i++ {
		if r := pick(t, twoLimitedAndUnlimited); r.AuthID != "u2" {
			t.Fatalf("pick u2 #%d = %+v", i, r)
		}
	}
	// all limited busy -> falls through to unlimited glm.
	if r := pick(t, twoLimitedAndUnlimited); r.AuthID != "glm" {
		t.Fatalf("fallback pick = %+v, want glm", r)
	}
	// release eps -> next pick takes eps again.
	usage(t, "eps")
	if r := pick(t, twoLimitedAndUnlimited); r.AuthID != "eps" {
		t.Fatalf("post-release pick = %+v, want eps", r)
	}
}

func TestNoLimitedCandidatesDelegates(t *testing.T) {
	r := pick(t, `[{"ID":"a","Provider":"devin","Priority":20}]`)
	if r.Handled {
		t.Fatalf("expected builtin delegation, got %+v", r)
	}
}

func TestPickCountTokensDelegatesWithoutAcquiringSlot(t *testing.T) {
	seedConfig(t, nil)
	defer func() { inflight.slots = map[string][]time.Time{} }()

	// count_tokens requests complete without usage records; acquiring a slot
	// here would hold the credential until slot_ttl.
	raw, err := handleMethod("scheduler.pick", []byte(`{"provider":"devin","options":{"metadata":{"request_path":"/v1/messages/count_tokens"}},"candidates":[{"ID":"eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}]}`))
	if err != nil {
		t.Fatalf("pickAuth error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	var resp pluginapiPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v", err)
	}
	if resp.Handled {
		t.Fatalf("count_tokens pick should delegate to builtin, got %+v", resp)
	}
	if len(inflight.slots["eps"]) != 0 {
		t.Fatalf("count_tokens pick acquired a slot: %v", inflight.slots["eps"])
	}
}

func TestHigherPriorityWinsEvenIfListedLast(t *testing.T) {
	seedConfig(t, nil)
	defer func() { inflight.slots = map[string][]time.Time{} }()
	// Host across-priority list is ID-sorted, so the cheap fallback can arrive first.
	cands := `[{"ID":"glm","Provider":"openrouter","Priority":5},{"ID":"eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}]`
	if r := pick(t, cands); r.AuthID != "eps" {
		t.Fatalf("pick = %+v, want eps ahead of glm", r)
	}
	r := pick(t, cands)
	if r.AuthID != "glm" {
		t.Fatalf("fallback pick = %+v, want glm", r)
	}
}

func TestAllBusyRejects(t *testing.T) {
	seedConfig(t, nil)
	defer func() {
		inflight.slots = map[string][]time.Time{}
		inflight.lastPicked = map[string]string{}
	}()
	cands := `[{"ID":"eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}]`
	if r := pick(t, cands); r.AuthID != "eps" {
		t.Fatalf("pick = %+v", r)
	}
	r := pick(t, cands)
	if !r.Reject || !strings.Contains(r.RejectCode, "unavailable") {
		t.Fatalf("expected auth_unavailable reject, got %+v", r)
	}
}

func TestPickRoundRobinAlternatesWithinTier(t *testing.T) {
	seedConfig(t, map[string]bool{"devin": true})
	defer func() {
		inflight.slots = map[string][]time.Time{}
		inflight.lastPicked = map[string]string{}
	}()
	// cle (ID 01) and eps (ID 02) both have priority 20 and limit 1.
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`

	// Pick 1: should pick 01-cle
	r1 := pick(t, cands)
	if r1.AuthID != "01-cle" {
		t.Fatalf("pick 1 = %s, want 01-cle", r1.AuthID)
	}
	usage(t, "01-cle")

	// Pick 2: should alternate to 02-eps
	r2 := pick(t, cands)
	if r2.AuthID != "02-eps" {
		t.Fatalf("pick 2 = %s, want 02-eps", r2.AuthID)
	}
	usage(t, "02-eps")

	// Pick 3: should alternate back to 01-cle
	r3 := pick(t, cands)
	if r3.AuthID != "01-cle" {
		t.Fatalf("pick 3 = %s, want 01-cle", r3.AuthID)
	}
	usage(t, "01-cle")

	// Pick 4: should alternate to 02-eps
	r4 := pick(t, cands)
	if r4.AuthID != "02-eps" {
		t.Fatalf("pick 4 = %s, want 02-eps", r4.AuthID)
	}
	usage(t, "02-eps")
}

func TestPickRoundRobinSkipsBusyAndResumes(t *testing.T) {
	seedConfig(t, map[string]bool{"devin": true})
	defer func() {
		inflight.slots = map[string][]time.Time{}
		inflight.lastPicked = map[string]string{}
	}()
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`

	// Pick 1: picks 01-cle (kept busy, not released)
	r1 := pick(t, cands)
	if r1.AuthID != "01-cle" {
		t.Fatalf("pick 1 = %s, want 01-cle", r1.AuthID)
	}

	// Pick 2: 02-eps is next in rotation and available
	r2 := pick(t, cands)
	if r2.AuthID != "02-eps" {
		t.Fatalf("pick 2 = %s, want 02-eps", r2.AuthID)
	}

	// Pick 3: both are busy -> rejected
	r3 := pick(t, cands)
	if !r3.Reject {
		t.Fatalf("pick 3 should be rejected when all busy, got %s", r3.AuthID)
	}

	// Release 01-cle
	usage(t, "01-cle")

	// Pick 4: 02-eps was last picked, next in ring is 01-cle which is now free
	r4 := pick(t, cands)
	if r4.AuthID != "01-cle" {
		t.Fatalf("pick 4 = %s, want 01-cle", r4.AuthID)
	}
}

func TestPickFillFirstSticksToFirstCredential(t *testing.T) {
	// antigravity is not in round_robin_providers: picks must stay on the first
	// credential until it reaches its cap, so upstream caching stays warm.
	seedConfig(t, map[string]bool{"devin": true})
	defer func() {
		inflight.slots = map[string][]time.Time{}
		inflight.lastPicked = map[string]string{}
	}()
	cands := `[
		{"ID":"acc-a","Provider":"antigravity","Priority":10,"Metadata":{"max-concurrent":3}},
		{"ID":"acc-b","Provider":"antigravity","Priority":10,"Metadata":{"max-concurrent":3}}
	]`

	// Three concurrent picks all land on acc-a (cap 3), never rotating to acc-b.
	for i := 0; i < 3; i++ {
		if r := pick(t, cands); r.AuthID != "acc-a" {
			t.Fatalf("pick %d = %s, want sticky acc-a", i+1, r.AuthID)
		}
	}
	// acc-a full -> next pick spills to acc-b.
	if r := pick(t, cands); r.AuthID != "acc-b" {
		t.Fatalf("overflow pick = %s, want acc-b", r.AuthID)
	}
	// One slot frees on acc-a -> next pick goes straight back to acc-a, not acc-b.
	usage(t, "acc-a")
	if r := pick(t, cands); r.AuthID != "acc-a" {
		t.Fatalf("post-release pick = %s, want fill-first acc-a", r.AuthID)
	}
}

func TestPickRoundRobinConfiguredViaYAML(t *testing.T) {
	defer func() {
		inflight.slots = map[string][]time.Time{}
		inflight.lastPicked = map[string]string{}
		currentConfig.Store(pluginConfig{slotTTL: 30 * time.Minute})
	}()
	cfgYAML, _ := yaml.Marshal(map[string]any{
		"round_robin_providers": []string{"devin"},
	})
	req, _ := json.Marshal(map[string]any{"config_yaml": cfgYAML})
	raw, err := handleMethod("plugin.register", req)
	if err != nil {
		t.Fatalf("register error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("register not ok: %s", raw)
	}

	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`
	if r := pick(t, cands); r.AuthID != "01-cle" {
		t.Fatalf("pick 1 = %s, want 01-cle", r.AuthID)
	}
	usage(t, "01-cle")
	if r := pick(t, cands); r.AuthID != "02-eps" {
		t.Fatalf("pick 2 = %s, want 02-eps after YAML config", r.AuthID)
	}
}

func TestSweepStaleReclaimsExpiredSlots(t *testing.T) {
	resetInflight(t)
	now := time.Now()
	inflight.Lock()
	inflight.slots["x"] = []time.Time{now.Add(-2 * time.Hour), now}
	inflight.Unlock()

	sweepStale(now, 30*time.Minute)

	inflight.Lock()
	defer inflight.Unlock()
	if got := inflight.slots["x"]; len(got) != 1 || !got[0].Equal(now) {
		t.Fatalf("slots[x] = %v, want only the live stamp %v", got, now)
	}
	if inflight.swept["x"] != 1 {
		t.Fatalf("swept[x] = %d, want 1", inflight.swept["x"])
	}
}

func TestReleaseSlotPopsLiveStampBeforeSwept(t *testing.T) {
	resetInflight(t)
	now := time.Now()
	inflight.Lock()
	inflight.slots["x"] = []time.Time{now}
	inflight.swept["x"] = 1
	inflight.Unlock()

	usage(t, "x")

	inflight.Lock()
	defer inflight.Unlock()
	if got := inflight.slots["x"]; len(got) != 0 {
		t.Fatalf("slots[x] = %v, want the live stamp released", got)
	}
	if inflight.swept["x"] != 1 {
		t.Fatalf("swept[x] = %d, want credit kept for the still-missing record", inflight.swept["x"])
	}
}

func TestSweptRatchetDoesNotBrickCredential(t *testing.T) {
	seedConfig(t, nil)
	resetInflight(t)
	cands := `[{"ID":"x","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}]`

	// A picked slot whose usage record never arrives leaks; a pick after the TTL
	// sweeps it and acquires a fresh live stamp in the same call.
	inflight.Lock()
	inflight.slots["x"] = []time.Time{time.Now().Add(-time.Hour)}
	inflight.Unlock()
	if r := pick(t, cands); r.AuthID != "x" {
		t.Fatalf("pick after sweep = %+v, want x", r)
	}
	if got := inflight.swept["x"]; got != 1 {
		t.Fatalf("swept[x] = %d, want 1", got)
	}
	// The live request's usage record must release its own stamp even though a
	// swept credit lingers; otherwise the cred bricks until restart.
	usage(t, "x")
	inflight.Lock()
	got := len(inflight.slots["x"])
	inflight.Unlock()
	if got != 0 {
		t.Fatalf("slots[x] len = %d, want live stamp released (no ratchet)", got)
	}
	if r := pick(t, cands); r.AuthID != "x" {
		t.Fatalf("post-release pick = %+v, want x", r)
	}
}

func TestReleaseSlotPascalCaseAuthID(t *testing.T) {
	resetInflight(t)
	now := time.Now()
	inflight.Lock()
	inflight.slots["x"] = []time.Time{now, now}
	inflight.Unlock()

	raw, err := handleMethod("usage.handle", []byte(`{"AuthID":"x"}`))
	if err != nil {
		t.Fatalf("usage error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("usage not ok: %s", raw)
	}

	inflight.Lock()
	defer inflight.Unlock()
	if got := inflight.slots["x"]; len(got) != 1 {
		t.Fatalf("slots[x] = %v, want one stamp popped via PascalCase AuthID", got)
	}
}

func TestConfigureRejectsNonPositiveSlotTTL(t *testing.T) {
	for _, ttl := range []string{"0s", "-5m"} {
		cfgYAML, _ := yaml.Marshal(map[string]any{"slot_ttl": ttl})
		req, _ := json.Marshal(map[string]any{"config_yaml": cfgYAML})
		_, err := handleMethod("plugin.register", req)
		if err == nil {
			t.Fatalf("slot_ttl %q: expected error, got nil", ttl)
		}
		if !strings.Contains(err.Error(), "slot_ttl must be a positive duration") {
			t.Fatalf("slot_ttl %q: unexpected error: %v", ttl, err)
		}
	}
	// Restore a valid config so other tests are unaffected.
	t.Cleanup(func() { currentConfig.Store(pluginConfig{slotTTL: 30 * time.Minute}) })
}

// seedAffinityConfig enables session affinity (and optionally round-robin).
func seedAffinityConfig(t *testing.T, rrProviders map[string]bool) {
	t.Helper()
	currentConfig.Store(pluginConfig{
		slotTTL:         30 * time.Minute,
		rrProviders:     rrProviders,
		affinityTTL:     time.Hour,
		SessionAffinity: true,
	})
	t.Cleanup(func() { currentConfig.Store(pluginConfig{slotTTL: 30 * time.Minute}) })
}

// pickWithSession issues a scheduler.pick carrying a session identity header.
func pickWithSession(t *testing.T, candidates, header, sessionID string) pluginapiPickResponse {
	t.Helper()
	body := map[string]any{
		"provider":   "devin",
		"model":      "claude-fable-5-1",
		"candidates": json.RawMessage(candidates),
		"options": map[string]any{
			"headers": map[string][]string{header: {sessionID}},
		},
	}
	rawReq, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	raw, err := handleMethod("scheduler.pick", rawReq)
	if err != nil {
		t.Fatalf("pickAuth error: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("pick not ok: %s", raw)
	}
	var resp pluginapiPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v", err)
	}
	return resp
}

func TestSessionAffinityPinsCredentialAcrossTurns(t *testing.T) {
	// Round-robin would alternate cle<->eps; affinity must keep one session on
	// the credential holding its cached prefix.
	seedAffinityConfig(t, map[string]bool{"devin": true})
	resetInflight(t)
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`

	first := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "session-a")
	usage(t, first.AuthID)

	for i := 0; i < 3; i++ {
		got := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "session-a")
		if got.AuthID != first.AuthID {
			t.Fatalf("turn %d picked %s, want pinned %s", i+2, got.AuthID, first.AuthID)
		}
		usage(t, got.AuthID)
	}

	// A different session is free to land on the other credential.
	other := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "session-b")
	if other.AuthID == first.AuthID {
		t.Fatalf("second session also pinned to %s; affinity over-applied", other.AuthID)
	}
}

func TestSessionAffinitySubagentInheritsParent(t *testing.T) {
	seedAffinityConfig(t, map[string]bool{"devin": true})
	resetInflight(t)
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`

	parent := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "root-task")
	usage(t, parent.AuthID)

	// Subagent carries its own session id but points at the parent.
	sub := pickWithSession(t, cands, "X-Parent-Session-Id", "root-task")
	if sub.AuthID != parent.AuthID {
		t.Fatalf("subagent picked %s, want parent's %s", sub.AuthID, parent.AuthID)
	}

	// ":agent:" suffixed ids fold onto the parent too.
	usage(t, sub.AuthID)
	sub2 := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "root-task:agent:worker-1")
	if sub2.AuthID != parent.AuthID {
		t.Fatalf("agent-suffixed session picked %s, want parent's %s", sub2.AuthID, parent.AuthID)
	}
}

func TestSessionAffinityKeepsPinWhenBoundCredentialBusy(t *testing.T) {
	seedAffinityConfig(t, nil)
	resetInflight(t)
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`

	first := pickWithSession(t, cands, "X-Session-ID", "session-busy")
	// Leave the slot held so the pinned credential is at its cap.
	spill := pickWithSession(t, cands, "X-Session-ID", "session-busy")
	if spill.AuthID == first.AuthID {
		t.Fatalf("expected spill to a peer while %s is busy, got %s", first.AuthID, spill.AuthID)
	}
	// The pin must survive the transient spill: once the original frees up the
	// session returns to it instead of drifting permanently.
	usage(t, first.AuthID)
	usage(t, spill.AuthID)
	back := pickWithSession(t, cands, "X-Session-ID", "session-busy")
	if back.AuthID != first.AuthID {
		t.Fatalf("after release picked %s, want the original pin %s", back.AuthID, first.AuthID)
	}
}

func TestSessionAffinityOffKeepsRoundRobin(t *testing.T) {
	// Default config has affinity disabled: behaviour must match the old RR path.
	seedConfig(t, map[string]bool{"devin": true})
	resetInflight(t)
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"02-eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}
	]`

	first := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "session-rr")
	usage(t, first.AuthID)
	second := pickWithSession(t, cands, "X-Claude-Code-Session-Id", "session-rr")
	if second.AuthID == first.AuthID {
		t.Fatalf("affinity disabled but picks stayed on %s; RR broken", first.AuthID)
	}
}

func TestSweepBindingsDropsExpiredPins(t *testing.T) {
	resetInflight(t)
	now := time.Now()
	inflight.Lock()
	inflight.bindings["k"] = affinityBinding{authID: "x", at: now.Add(-2 * time.Hour)}
	inflight.bindings["fresh"] = affinityBinding{authID: "y", at: now}
	inflight.Unlock()

	sweepBindings(now, time.Hour)

	inflight.Lock()
	defer inflight.Unlock()
	if _, ok := inflight.bindings["k"]; ok {
		t.Fatalf("expired binding survived the sweep")
	}
	if _, ok := inflight.bindings["fresh"]; !ok {
		t.Fatalf("live binding was dropped")
	}
}

func TestSessionAffinityDoesNotBindFallbackTier(t *testing.T) {
	// A session that starts while the preferred tier is exhausted must not be
	// pinned to the paid fallback; it should return to the top tier once free.
	seedAffinityConfig(t, nil)
	resetInflight(t)
	cands := `[
		{"ID":"01-cle","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}},
		{"ID":"glm","Provider":"devin","Priority":14,"Metadata":{"max-concurrent":4}}
	]`

	// Hold the top tier busy with another session.
	blocker := pickWithSession(t, cands, "X-Session-ID", "blocker")
	if blocker.AuthID != "01-cle" {
		t.Fatalf("blocker = %s, want 01-cle", blocker.AuthID)
	}

	// Our session starts during the outage and lands on the fallback.
	first := pickWithSession(t, cands, "X-Session-ID", "late-session")
	if first.AuthID != "glm" {
		t.Fatalf("late session = %s, want fallback glm", first.AuthID)
	}
	usage(t, first.AuthID)

	// Preferred tier frees up: the session must go back, not stay on the pin.
	usage(t, blocker.AuthID)
	next := pickWithSession(t, cands, "X-Session-ID", "late-session")
	if next.AuthID != "01-cle" {
		t.Fatalf("after recovery picked %s, want top tier 01-cle", next.AuthID)
	}
}
