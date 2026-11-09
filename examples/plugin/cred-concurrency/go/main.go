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
}{
	slots:      make(map[string][]time.Time),
	lastPicked: make(map[string]string),
	swept:      make(map[string]int),
	bindings:   make(map[string]affinityBinding),
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

	slotTTL     time.Duration
	rrProviders map[string]bool
	affinityTTL time.Duration
	waitTimeout time.Duration
}

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
			Version:          "0.1.0",
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
			},
		},
		Capabilities: registrationCapability{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			UsagePlugin:               true,
		},
	}
}

// limitFor resolves the concurrency cap for a candidate: config override first,
// then the credential's max_concurrent metadata/attribute. 0 means unlimited.
func limitFor(cfg pluginConfig, candidate pluginapi.SchedulerAuthCandidate) int64 {
	if cfg.Limits != nil {
		if limit, ok := cfg.Limits[candidate.ID]; ok {
			return limit
		}
	}
	for _, key := range []string{"max_concurrent", "max-concurrent"} {
		if value, ok := candidate.Metadata[key]; ok {
			if limit, ok := metadataLimit(value); ok {
				return limit
			}
		}
		if raw := strings.TrimSpace(candidate.Attributes[key]); raw != "" {
			if limit, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
				return max(limit, 0)
			}
		}
	}
	return 0
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

func acquireSlot(candidate pluginapi.SchedulerAuthCandidate, limit int64, rotKey string, now time.Time) bool {
	if limit > 0 {
		if int64(len(inflight.slots[candidate.ID])) >= limit {
			return false
		}
		inflight.slots[candidate.ID] = append(inflight.slots[candidate.ID], now)
	}
	if inflight.lastPicked != nil && rotKey != "" {
		inflight.lastPicked[rotKey] = candidate.ID
	}
	return true
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

	// Fast path: no limited candidate at all -> let the builtin scheduler run.
	hasLimited := false
	for _, candidate := range req.Candidates {
		if limitFor(cfg, candidate) > 0 {
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
					if acquireSlot(candidate, limitFor(cfg, candidate), tierRotationKey(candidate), now) {
						return pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true}, nil
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
			if !acquireSlot(candidate, limitFor(cfg, candidate), rotKey, now) {
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
	inflight.Lock()
	defer inflight.Unlock()
	stamps := inflight.slots[authID]
	if len(stamps) > 0 {
		// Live stamps release first: a record almost always belongs to a live
		// pick. A swept slot's record rarely arrives later (requests do not
		// outlive the TTL by much), and eating live records while swept credit
		// exists strands every future stamp behind the TTL.
		if len(stamps) == 1 {
			delete(inflight.slots, authID)
		} else {
			inflight.slots[authID] = stamps[1:]
		}
		return okEnvelope(struct{}{})
	}
	// No live stamps: the record belongs to a slot already reclaimed as a leak.
	if inflight.swept[authID] > 0 {
		inflight.swept[authID]--
		if inflight.swept[authID] <= 0 {
			delete(inflight.swept, authID)
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
