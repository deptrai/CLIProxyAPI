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
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var currentConfig atomic.Value

// inflight tracks acquired slots per auth ID. Each entry is one acquire timestamp;
// entries older than the configured TTL are treated as stale leaks and dropped.
// lastPicked preserves round-robin rotation within each priority tier, but only
// for providers listed in round_robin_providers; other providers stay fill-first.
var inflight = struct {
	sync.Mutex
	slots      map[string][]time.Time
	lastPicked map[string]string
	// swept counts slots per auth ID that sweepStale reclaimed as leaks. A usage
	// record arriving while no live stamps exist is assumed to belong to a swept
	// slot and retires one credit. Live stamps always release on their own
	// records first: a leaked slot's record usually never arrives at all, so
	// letting swept outrank live stamps bricks the credential until restart.
	swept map[string]int
	// bindings pins a session (per provider+model) to one credential. Upstream
	// prefix caches live on the serving account, so spreading one conversation
	// across accounts throws the cached prefix away on every hop.
	bindings map[string]affinityBinding
	// breakers holds the per-auth slow-breaker state for slow_breakers rules.
	breakers map[string]*breakerState
}{
	slots:      make(map[string][]time.Time),
	lastPicked: make(map[string]string),
	swept:      make(map[string]int),
	bindings:   make(map[string]affinityBinding),
	breakers:   make(map[string]*breakerState),
}

// affinityBinding is one session-to-credential pin with the time it was made.
type affinityBinding struct {
	authID string
	at     time.Time
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

type pluginConfig struct {
	// Limits optionally overrides per-auth limits by auth ID. Credentials may also
	// carry a limit via the "max_concurrent" attribute in their auth JSON.
	Limits map[string]int64 `yaml:"limits"`
	// SlotTTL bounds how long an acquired slot may live without a matching usage
	// record; it only guards against leaks when a picked auth never executes.
	// Defaults to 30m.
	SlotTTL string `yaml:"slot_ttl"`
	// RoundRobinProviders lists provider names that rotate picks within a
	// priority tier (e.g. to spread quota across accounts). Providers not
	// listed keep fill-first ordering: the first credential under its cap is
	// reused, which preserves upstream prefix caching on a single account.
	RoundRobinProviders []string `yaml:"round_robin_providers"`
	// SessionAffinity pins a session to the credential that served it first so
	// upstream prefix caches stay warm across turns. Subagents inherit their
	// parent session's binding via X-Parent-Session-Id. Off by default because
	// the host only reaches this plugin when the builtin selector is bypassed.
	SessionAffinity bool `yaml:"session_affinity"`
	// SessionAffinityTTL bounds how long a binding survives without a fresh
	// pick. Defaults to 1h.
	SessionAffinityTTL string `yaml:"session_affinity_ttl"`
	// WaitTimeout, when positive, makes the scheduler retry a rejected pick
	// (all limited credentials busy) until a slot frees or the timeout
	// elapses, instead of failing fast with 503. Each retry re-locks and
	// re-scans, so waiting requests do not block other scheduling decisions.
	// Zero or empty keeps the fail-fast behavior.
	WaitTimeout string `yaml:"wait_timeout"`

	// ModelLimits optionally caps concurrency per model group. All models in one
	// group share a single slot bucket per auth, so several upstream variants
	// (e.g. swe-2-flash medium/high) drain one budget. A matching rule overrides
	// the auth's base limit for that request. Model patterns are case-insensitive
	// and support a single trailing "*" for prefix matches.
	ModelLimits []modelLimitRule `yaml:"model_limits"`

	// SlowBreakers optionally trips a per-auth circuit breaker when an upstream
	// gets slow: min_samples consecutive completions over threshold_ms close the
	// auth for open_seconds, while one probe request leaks through every
	// probe_seconds — a fast probe reopens the auth early. Metric is TTFT when
	// recorded, else total request latency; failed requests always count slow.
	SlowBreakers []slowBreakerRule `yaml:"slow_breakers"`

	slotTTL     time.Duration
	rrProviders map[string]bool
	affinityTTL time.Duration
	waitTimeout time.Duration
}

type modelLimitRule struct {
	// Key names the shared slot bucket; models in the same group share it.
	Key string `yaml:"key"`
	// Limit is the max concurrent requests per auth for this group.
	Limit int64 `yaml:"limit"`
	// Providers optionally scopes the rule to candidates of these providers.
	Providers []string `yaml:"providers"`
	// Models lists requested-model patterns; trailing "*" means prefix match.
	Models []string `yaml:"models"`
}

// slowBreakerRule configures a per-auth latency circuit breaker.
type slowBreakerRule struct {
	// Providers scopes the rule to these provider names (case-insensitive
	// match on the candidate's / usage record's Provider).
	Providers []string `yaml:"providers"`
	// ThresholdMs is the latency in ms above which a completion counts as
	// slow. TTFT is used when recorded, else total request latency.
	ThresholdMs int64 `yaml:"threshold_ms"`
	// MinSamples is how many consecutive slow completions trip the breaker
	// (default 3).
	MinSamples int `yaml:"min_samples"`
	// OpenSeconds is how long the breaker stays open (default 300).
	OpenSeconds int64 `yaml:"open_seconds"`
	// ProbeSeconds is the minimum interval between probe requests while the
	// breaker is open (default 60).
	ProbeSeconds int64 `yaml:"probe_seconds"`
}

// breakerState is the per-auth slow-breaker state: consecutive slow samples
// while closed, and while open the probe bookkeeping (only one probe leaks
// through every probe_seconds; a fast probe closes the breaker early).
type breakerState struct {
	slow           int
	openUntil      time.Time
	probeAt        time.Time
	probing        bool
	probeGrantedAt time.Time
}

// probeStaleTTL is how long a granted probe may go without a usage record
// before a fresh probe is allowed again (e.g. client disconnect).
const probeStaleTTL = 2 * time.Minute

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	UsagePlugin               bool `json:"usage_plugin"`
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
	case pluginabi.MethodSchedulerPick:
		return pickAuth(request)
	case pluginabi.MethodUsageHandle:
		return releaseSlot(request)
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

	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	cfg.slotTTL = 30 * time.Minute
	if trimmed := strings.TrimSpace(cfg.SlotTTL); trimmed != "" {
		parsed, errParse := time.ParseDuration(trimmed)
		if errParse != nil {
			return errParse
		}
		cfg.slotTTL = parsed
	}
	if cfg.slotTTL <= 0 {
		return errors.New("slot_ttl must be a positive duration")
	}
	cfg.affinityTTL = time.Hour
	if trimmed := strings.TrimSpace(cfg.SessionAffinityTTL); trimmed != "" {
		parsed, errParse := time.ParseDuration(trimmed)
		if errParse != nil {
			return errParse
		}
		cfg.affinityTTL = parsed
	}
	if cfg.affinityTTL <= 0 {
		return errors.New("session_affinity_ttl must be a positive duration")
	}
	cfg.waitTimeout = 0
	if trimmed := strings.TrimSpace(cfg.WaitTimeout); trimmed != "" {
		parsed, errParse := time.ParseDuration(trimmed)
		if errParse != nil {
			return errParse
		}
		if parsed < 0 {
			return errors.New("wait_timeout must not be negative")
		}
		cfg.waitTimeout = parsed
	}
	cfg.rrProviders = map[string]bool{}
	for _, provider := range cfg.RoundRobinProviders {
		if trimmed := strings.TrimSpace(provider); trimmed != "" {
			cfg.rrProviders[trimmed] = true
		}
	}
	currentConfig.Store(cfg)
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	var cfg pluginConfig
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	return cfg, nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "cred-concurrency",
			Version:          "0.2.0",
			Author:           "deptrai",
			GitHubRepository: "https://github.com/deptrai/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "limits",
					Type:        pluginapi.ConfigFieldTypeObject,
					Description: "Optional per-auth concurrency limits keyed by auth ID. Overrides the auth's max_concurrent attribute.",
				},
				{
					Name:        "slot_ttl",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "How long an acquired slot may live without a usage record before it is reclaimed (default 30m).",
				},
				{
					Name:        "round_robin_providers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Providers that rotate picks within a priority tier (e.g. [devin] to spread quota). Unlisted providers keep fill-first order so one credential stays hot for upstream caching.",
				},
				{
					Name:        "session_affinity",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Pin each session (and its subagents, via X-Parent-Session-Id) to the credential that served it first so upstream prefix caches stay warm. Default false.",
				},
				{
					Name:        "session_affinity_ttl",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "How long a session-to-credential pin survives without a fresh pick (default 1h).",
				},
				{
					Name:        "model_limits",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Optional per-model-group concurrency caps: [{key, limit, providers?, models}]. Models in one group share a single slot bucket per auth; trailing \"*\" in a pattern is a prefix match.",
				},
				{
					Name:        "slow_breakers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Optional per-auth latency circuit breakers: [{providers, threshold_ms, min_samples?, open_seconds?, probe_seconds?}]. min_samples consecutive slow completions (TTFT, else latency; failures count) close the auth for open_seconds; one probe leaks through every probe_seconds and a fast probe reopens early.",
				},
			},
		},
		Capabilities: registrationCapability{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			UsagePlugin:               true,
		},
	}
}

// matchModelPattern reports whether model matches pattern; a trailing "*" is a
// case-insensitive prefix wildcard, anything else is an exact match.
func matchModelPattern(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(strings.ToLower(model), strings.ToLower(pattern[:len(pattern)-1]))
	}
	return strings.EqualFold(pattern, model)
}

// matchModelLimit returns the first model_limits rule applying to this
// candidate + model pair, or nil. Rule providers empty means all providers.
func matchModelLimit(cfg pluginConfig, candidate pluginapi.SchedulerAuthCandidate, model string) *modelLimitRule {
	for i := range cfg.ModelLimits {
		rule := &cfg.ModelLimits[i]
		// Rules with a non-positive limit place no stamps anywhere: matching
		// them at release time would point the pop at a bucket that never
		// exists while pick stored under the auth-limit bucket.
		if rule.Limit <= 0 {
			continue
		}
		if len(rule.Providers) > 0 {
			matched := false
			for _, provider := range rule.Providers {
				if strings.EqualFold(strings.TrimSpace(provider), strings.TrimSpace(candidate.Provider)) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		for _, pattern := range rule.Models {
			if matchModelPattern(pattern, model) {
				return rule
			}
		}
	}
	return nil
}

// limitFor resolves the concurrency cap for a candidate: a matching
// model_limits rule wins first (its group key becomes the slot bucket), then
// the per-auth config override, then the credential's max_concurrent
// metadata/attribute. 0 means unlimited. The returned bucket scopes the slot
// count: "" = per-auth bucket (legacy behavior), otherwise "<rule key>".
func limitFor(cfg pluginConfig, candidate pluginapi.SchedulerAuthCandidate, model string) (int64, string) {
	if rule := matchModelLimit(cfg, candidate, model); rule != nil && rule.Limit > 0 {
		return rule.Limit, rule.Key
	}
	if cfg.Limits != nil {
		if limit, ok := cfg.Limits[candidate.ID]; ok {
			return limit, ""
		}
	}
	for _, key := range []string{"max_concurrent", "max-concurrent"} {
		if value, ok := candidate.Metadata[key]; ok {
			if limit, ok := metadataLimit(value); ok {
				return limit, ""
			}
		}
		if raw := strings.TrimSpace(candidate.Attributes[key]); raw != "" {
			if limit, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
				return max(limit, 0), ""
			}
		}
	}
	return 0, ""
}

// matchSlowBreaker returns the first slow_breakers rule covering provider, or
// nil. Provider matching is case-insensitive.
func matchSlowBreaker(cfg pluginConfig, provider string) *slowBreakerRule {
	want := strings.ToLower(strings.TrimSpace(provider))
	if want == "" {
		return nil
	}
	for i := range cfg.SlowBreakers {
		rule := &cfg.SlowBreakers[i]
		if rule.ThresholdMs <= 0 {
			continue
		}
		for _, p := range rule.Providers {
			if strings.ToLower(strings.TrimSpace(p)) == want {
				return rule
			}
		}
	}
	return nil
}

// breakerAllows reports whether the candidate may be picked while its slow
// breaker is open. The first pick after probe_seconds marks the auth as
// probing; concurrent probes are refused. Callers must hold inflight.Lock.
func breakerAllows(rule *slowBreakerRule, authID string, now time.Time) bool {
	if rule == nil {
		return true
	}
	st := inflight.breakers[authID]
	if st == nil || !st.openUntil.After(now) {
		return true
	}
	if now.Before(st.probeAt) {
		return false
	}
	if st.probing && now.Sub(st.probeGrantedAt) < probeStaleTTL {
		return false
	}
	st.probing = true
	st.probeGrantedAt = now
	st.probeAt = now.Add(breakerProbeInterval(rule))
	return true
}

// recordBreakerSample folds a completed request's latency into the breaker
// state. Metric: TTFT when recorded, else total request latency. Failures
// always count as slow. Callers must hold inflight.Lock.
func recordBreakerSample(cfg pluginConfig, record pluginapi.UsageRecord, now time.Time) {
	rule := matchSlowBreaker(cfg, record.Provider)
	if rule == nil || record.AuthID == "" {
		return
	}
	st := inflight.breakers[record.AuthID]
	if st == nil {
		st = &breakerState{}
		inflight.breakers[record.AuthID] = st
	}
	dur := record.TTFT
	if dur <= 0 {
		dur = record.Latency
	}
	slow := record.Failed || dur.Milliseconds() > rule.ThresholdMs
	if st.openUntil.After(now) {
		// Open: only the probe result counts. A completion that started before
		// the probe was granted is an in-flight leftover and must not be read as
		// the probe — a fast leftover would otherwise close a still-slow breaker.
		if st.probing && isProbeCompletion(record, st, now) {
			st.probing = false
			if slow {
				st.probeAt = now.Add(breakerProbeInterval(rule))
			} else {
				st.openUntil = time.Time{}
				st.slow = 0
			}
		}
		return
	}
	if !slow {
		st.slow = 0
		return
	}
	st.slow++
	minSamples := rule.MinSamples
	if minSamples < 1 {
		minSamples = 3
	}
	if st.slow >= minSamples {
		openSeconds := rule.OpenSeconds
		if openSeconds < 1 {
			openSeconds = 300
		}
		st.openUntil = now.Add(time.Duration(openSeconds) * time.Second)
		st.probeAt = now.Add(breakerProbeInterval(rule))
		st.slow = 0
	}
}

// isProbeCompletion reports whether a usage record belongs to the granted
// probe rather than a request that was already in flight when the probe
// started: the request's approximate start (completion minus total latency)
// must be at/after the grant. Callers must hold inflight.Lock.
func isProbeCompletion(record pluginapi.UsageRecord, st *breakerState, now time.Time) bool {
	dur := record.Latency
	if dur <= 0 {
		dur = record.TTFT
	}
	startedAt := now.Add(-dur)
	return !startedAt.Before(st.probeGrantedAt.Add(-1 * time.Second))
}

// breakerProbeInterval returns the probe spacing with its default applied.
func breakerProbeInterval(rule *slowBreakerRule) time.Duration {
	if rule.ProbeSeconds > 0 {
		return time.Duration(rule.ProbeSeconds) * time.Second
	}
	return 60 * time.Second
}

// metadataLimit normalizes numeric metadata values (JSON decodes to float64).
func metadataLimit(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case string:
		limit, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return limit, errParse == nil
	}
	return 0, false
}

// sweepStale drops acquired slots that never produced a usage record within the TTL.
func sweepStale(now time.Time, ttl time.Duration) {
	for authID, stamps := range inflight.slots {
		kept := stamps[:0]
		for _, ts := range stamps {
			if now.Sub(ts) < ttl {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(inflight.slots, authID)
		} else {
			inflight.slots[authID] = kept
		}
		// Dropped stamps are counted as leaked slots; their usage records (if any
		// arrive late) must not pop live stamps in releaseSlot.
		if dropped := len(stamps) - len(kept); dropped > 0 {
			inflight.swept[authID] += dropped
		}
	}
}

// affinitySessionHeaders mirrors the host's session extraction order so a
// session keeps one identity whether the builtin selector or this plugin picks.
var affinitySessionHeaders = []string{
	"X-Claude-Code-Session-Id",
	"Session-Id",
	"X-Http-Session-Id",
	"X-Session-ID",
	"X-Session-Affinity",
	"X-Slot-Session-Id",
}

func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// stripSubagentSuffix folds "<parent>:agent:<id>" onto its parent so subagents
// share the conversation's credential instead of pinning their own.
func stripSubagentSuffix(sessionID string) string {
	if idx := strings.Index(sessionID, ":agent:"); idx > 0 {
		return sessionID[:idx]
	}
	return sessionID
}

// sessionIdentity resolves the affinity key for a request. A subagent's parent
// session wins over its own ID, matching session-affinity-subagents in the host.
func sessionIdentity(opts pluginapi.SchedulerOptions) string {
	if opts.Metadata != nil {
		if parent, ok := opts.Metadata["parent_session_id"].(string); ok {
			if trimmed := strings.TrimSpace(parent); trimmed != "" {
				return stripSubagentSuffix(trimmed)
			}
		}
		if canonical, ok := opts.Metadata["canonical_session_id"].(string); ok {
			if trimmed := strings.TrimSpace(canonical); trimmed != "" {
				return stripSubagentSuffix(trimmed)
			}
		}
	}
	if parent := headerValue(opts.Headers, "X-Parent-Session-Id"); parent != "" {
		return stripSubagentSuffix(parent)
	}
	for _, name := range affinitySessionHeaders {
		if sessionID := headerValue(opts.Headers, name); sessionID != "" {
			return stripSubagentSuffix(sessionID)
		}
	}
	return ""
}

// sweepBindings drops pins that outlived the affinity TTL so a long-dead
// session cannot keep a credential reserved forever.
func sweepBindings(now time.Time, ttl time.Duration) {
	for key, binding := range inflight.bindings {
		if now.Sub(binding.at) >= ttl {
			delete(inflight.bindings, key)
		}
	}
}

// slotKey scopes the inflight bucket: model-limit groups get their own bucket
// per auth so e.g. swe-2(max) and swe-2-flash don't share a cap. The bucket is
// separated by a byte that can never appear in auth IDs or group keys.
func slotKey(authID, bucket string) string {
	if bucket == "" {
		return authID
	}
	return authID + "\x1f" + bucket
}

func acquireSlot(candidate pluginapi.SchedulerAuthCandidate, limit int64, bucket, rotKey string, now time.Time) bool {
	if limit > 0 {
		key := slotKey(candidate.ID, bucket)
		if int64(len(inflight.slots[key])) >= limit {
			return false
		}
		inflight.slots[key] = append(inflight.slots[key], now)
	}
	if inflight.lastPicked != nil && rotKey != "" {
		inflight.lastPicked[rotKey] = candidate.ID
	}
	return true
}

// undoAcquireSlot removes the stamp a just-succeeded acquireSlot appended for
// now when a later gate (e.g. the slow breaker) rejects the candidate — without
// it the breaker probe is burned while no request ever runs. Callers must hold
// inflight.Lock.
func undoAcquireSlot(candidate pluginapi.SchedulerAuthCandidate, bucket string, now time.Time) {
	key := slotKey(candidate.ID, bucket)
	stamps := inflight.slots[key]
	if n := len(stamps); n > 0 && stamps[n-1].Equal(now) {
		stamps = stamps[:n-1]
		if len(stamps) == 0 {
			delete(inflight.slots, key)
		} else {
			inflight.slots[key] = stamps
		}
	}
}

func tierRotationKey(candidate pluginapi.SchedulerAuthCandidate) string {
	rotKey := strconv.Itoa(candidate.Priority)
	if provider := strings.TrimSpace(candidate.Provider); provider != "" {
		rotKey = provider + ":" + rotKey
	}
	return rotKey
}

func pickAuth(raw []byte) ([]byte, error) {
	cfg := loadedConfig()
	if cfg.waitTimeout <= 0 {
		resp, errPick := pickAuthAttempt(raw)
		if errPick != nil {
			return nil, errPick
		}
		return okEnvelope(resp)
	}
	deadline := time.Now().Add(cfg.waitTimeout)
	for {
		resp, errPick := pickAuthAttempt(raw)
		if errPick != nil {
			return nil, errPick
		}
		if !resp.Reject || time.Now().Add(250*time.Millisecond).After(deadline) {
			return okEnvelope(resp)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func pickAuthAttempt(raw []byte) (pluginapi.SchedulerPickResponse, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return pluginapi.SchedulerPickResponse{}, errUnmarshal
	}
	// Token-count routes never emit usage records, so any slot acquired for
	// them would be held until slot_ttl and brick the credential. Defer to the
	// builtin scheduler (still priority-ordered) without consuming a slot.
	if path, ok := req.Options.Metadata["request_path"].(string); ok &&
		strings.HasSuffix(path, "/count_tokens") {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	cfg := loadedConfig()

	// Fast path: no limited or breaker-scoped candidate at all -> let the
	// builtin scheduler run. Session affinity alone still needs the plugin
	// (it owns the pin store), so it engages the scheduler even with no caps.
	hasLimited := cfg.SessionAffinity
	for _, candidate := range req.Candidates {
		limit, _ := limitFor(cfg, candidate, req.Model)
		if limit > 0 || matchSlowBreaker(cfg, candidate.Provider) != nil {
			hasLimited = true
			break
		}
	}
	if !hasLimited {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	now := time.Now()
	inflight.Lock()
	defer inflight.Unlock()
	sweepStale(now, cfg.slotTTL)

	// Session affinity runs before tier ordering: a conversation must stay on
	// the account holding its cached prefix even when a peer tier exists.
	bindKey := ""
	pinnedBusy := false
	hadBinding := false
	if cfg.SessionAffinity {
		sweepBindings(now, cfg.affinityTTL)
		if sessionID := sessionIdentity(req.Options); sessionID != "" {
			providerKey := strings.ToLower(strings.TrimSpace(req.Provider))
			if providerKey == "" {
				providerKey = "mixed"
			}
			bindKey = providerKey + "::" + sessionID + "::" + strings.ToLower(strings.TrimSpace(req.Model))
			if binding, ok := inflight.bindings[bindKey]; ok {
				hadBinding = true
				for _, candidate := range req.Candidates {
					if candidate.ID != binding.authID {
						continue
					}
					// Busy is transient: fail over for this request but keep the
					// pin so the session returns once the slot frees.
					pinLimit, pinBucket := limitFor(cfg, candidate, req.Model)
					// Acquire before the breaker check: a breaker reject without a
					// slot acquire would burn the probe with no request ever sent.
					if acquireSlot(candidate, pinLimit, pinBucket, tierRotationKey(candidate), now) {
						if breakerAllows(matchSlowBreaker(cfg, candidate.Provider), candidate.ID, now) {
							return pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true}, nil
						}
						undoAcquireSlot(candidate, pinBucket, now)
					}
					pinnedBusy = true
					break
				}
			}
		}
	}

	// Host sends every priority tier (scheduler_across_priorities) sorted by ID,
	// not by priority. Reorder so a full high-priority credential falls through
	// to the next tier instead of an unlimited lower-priority one jumping the queue.
	sort.SliceStable(req.Candidates, func(i, j int) bool {
		return req.Candidates[i].Priority > req.Candidates[j].Priority
	})

	type priorityTier struct {
		priority   int
		candidates []pluginapi.SchedulerAuthCandidate
	}
	var tiers []priorityTier
	for _, candidate := range req.Candidates {
		if len(tiers) == 0 || tiers[len(tiers)-1].priority != candidate.Priority {
			tiers = append(tiers, priorityTier{
				priority:   candidate.Priority,
				candidates: []pluginapi.SchedulerAuthCandidate{candidate},
			})
		} else {
			tiers[len(tiers)-1].candidates = append(tiers[len(tiers)-1].candidates, candidate)
		}
	}
	// Only the top tier may create a pin. Binding a fallback would trap a
	// session on a paid lower tier for the whole TTL even after the preferred
	// credential frees up.
	topPriority := req.Candidates[0].Priority

	for _, tier := range tiers {
		m := len(tier.candidates)
		if m == 0 {
			continue
		}
		rotKey := tierRotationKey(tier.candidates[0])
		startIdx := 0
		if cfg.rrProviders[strings.TrimSpace(tier.candidates[0].Provider)] {
			if lastID, ok := inflight.lastPicked[rotKey]; ok && lastID != "" {
				for idx, cand := range tier.candidates {
					if cand.ID == lastID {
						startIdx = (idx + 1) % m
						break
					}
				}
			}
		}

		for i := 0; i < m; i++ {
			candidate := tier.candidates[(startIdx+i)%m]
			limit, bucket := limitFor(cfg, candidate, req.Model)
			// Acquire before the breaker check so a breaker reject can undo the
			// stamp; checking the breaker first would burn its probe grant while
			// no request is actually in flight (slot-full case).
			if !acquireSlot(candidate, limit, bucket, rotKey, now) {
				continue
			}
			if !breakerAllows(matchSlowBreaker(cfg, candidate.Provider), candidate.ID, now) {
				undoAcquireSlot(candidate, bucket, now)
				continue
			}
			// Never move an existing pin to another credential: a dead primary
			// leaves the session unpinned for this request, so it snaps back as
			// soon as the original credential returns instead of staying on the
			// fallback for the rest of the TTL. Stale pins expire via affinityTTL.
			if bindKey != "" && !pinnedBusy && !hadBinding && candidate.Priority == topPriority {
				inflight.bindings[bindKey] = affinityBinding{authID: candidate.ID, at: now}
			}
			return pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true}, nil
		}
	}
	return pluginapi.SchedulerPickResponse{
		Handled:      true,
		Reject:       true,
		RejectCode:   "auth_unavailable",
		RejectReason: "all concurrency-limited credentials are busy",
	}, nil
}

func releaseSlot(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		// Tolerate snake_case payloads in case the wire format changes.
		var aux struct {
			AuthID string `json:"auth_id"`
		}
		if errAux := json.Unmarshal(raw, &aux); errAux == nil {
			authID = strings.TrimSpace(aux.AuthID)
		}
	}
	if authID == "" {
		return okEnvelope(struct{}{})
	}
	// Keep the resolved ID on the record so downstream consumers (breaker
	// samples) see it even when the wire used snake_case.
	record.AuthID = authID
	// The slot bucket must match the one chosen at pick time: model-limit rules
	// scope by candidate provider + requested model. The usage record carries
	// the provider; the requested model may sit in Alias when the upstream name
	// was remapped, so try Alias first then Model.
	bucket := ""
	if cfg := loadedConfig(); len(cfg.ModelLimits) > 0 {
		candidate := pluginapi.SchedulerAuthCandidate{Provider: record.Provider}
		model := record.Alias
		if model == "" {
			model = record.Model
		}
		if rule := matchModelLimit(cfg, candidate, model); rule != nil {
			bucket = rule.Key
		} else if model != record.Model {
			if rule = matchModelLimit(cfg, candidate, record.Model); rule != nil {
				bucket = rule.Key
			}
		}
	}
	key := slotKey(authID, bucket)
	inflight.Lock()
	defer inflight.Unlock()
	recordBreakerSample(loadedConfig(), record, time.Now())
	stamps := inflight.slots[key]
	if len(stamps) == 0 {
		// The pick-time bucket can fail to recompute here (the record carries
		// the upstream model name, the alias is empty, or rules changed since
		// the pick): fall back to the oldest live stamp under this auth so a
		// real release still frees a slot instead of leaking until slot_ttl.
		fallbackKey := ""
		var fallbackAt time.Time
		for otherKey, otherStamps := range inflight.slots {
			if len(otherStamps) == 0 || otherKey == key {
				continue
			}
			if otherKey != authID && !strings.HasPrefix(otherKey, authID+"\x1f") {
				continue
			}
			if fallbackKey == "" || otherStamps[0].Before(fallbackAt) {
				fallbackKey, fallbackAt = otherKey, otherStamps[0]
			}
		}
		if fallbackKey != "" {
			key, stamps = fallbackKey, inflight.slots[fallbackKey]
		}
	}
	if len(stamps) > 0 {
		// Live stamps release first: a record almost always belongs to a live
		// pick. A swept slot's record rarely arrives later (requests do not
		// outlive the TTL by much), and eating live records while swept credit
		// exists strands every future stamp behind the TTL.
		if len(stamps) == 1 {
			delete(inflight.slots, key)
		} else {
			inflight.slots[key] = stamps[1:]
		}
		return okEnvelope(struct{}{})
	}
	// No live stamps: the record belongs to a slot already reclaimed as a leak.
	if inflight.swept[key] > 0 {
		inflight.swept[key]--
		if inflight.swept[key] <= 0 {
			delete(inflight.swept, key)
		}
	}
	return okEnvelope(struct{}{})
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return pluginConfig{slotTTL: 30 * time.Minute, affinityTTL: time.Hour}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
