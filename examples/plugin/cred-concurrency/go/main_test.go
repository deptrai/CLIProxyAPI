package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

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

func TestPickRespectsLimitsAndFallsBack(t *testing.T) {
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

func TestAllBusyRejects(t *testing.T) {
	defer func() { inflight.slots = map[string][]time.Time{} }()
	cands := `[{"ID":"eps","Provider":"devin","Priority":20,"Metadata":{"max-concurrent":1}}]`
	if r := pick(t, cands); r.AuthID != "eps" {
		t.Fatalf("pick = %+v", r)
	}
	r := pick(t, cands)
	if !r.Reject || !strings.Contains(r.RejectCode, "unavailable") {
		t.Fatalf("expected auth_unavailable reject, got %+v", r)
	}
}
