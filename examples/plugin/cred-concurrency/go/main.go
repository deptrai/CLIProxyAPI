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
	// swept counts slots per auth ID that sweepStale reclaimed as leaks. A later
	// usage record for that auth ID is assumed to belong to a swept (leaked) slot
	// first, so releaseSlot never pops a live stamp for it. This biases toward
	// under-admitting (a live slot may be held until TTL) but never over-admits.
	swept map[string]int
}{
	slots:      make(map[string][]time.Time),
	lastPicked: make(map[string]string),
	swept:      make(map[string]int),
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

	slotTTL     time.Duration
	rrProviders map[string]bool
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

func pickAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
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
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	now := time.Now()
	inflight.Lock()
	defer inflight.Unlock()
	sweepStale(now, cfg.slotTTL)

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

	for _, tier := range tiers {
		m := len(tier.candidates)
		if m == 0 {
			continue
		}
		rotKey := strconv.Itoa(tier.priority)
		if len(tier.candidates) > 0 && strings.TrimSpace(tier.candidates[0].Provider) != "" {
			rotKey = strings.TrimSpace(tier.candidates[0].Provider) + ":" + rotKey
		}
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
			limit := limitFor(cfg, candidate)
			if limit <= 0 {
				if inflight.lastPicked != nil {
					inflight.lastPicked[rotKey] = candidate.ID
				}
				return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true})
			}
			if int64(len(inflight.slots[candidate.ID])) < limit {
				inflight.slots[candidate.ID] = append(inflight.slots[candidate.ID], now)
				if inflight.lastPicked != nil {
					inflight.lastPicked[rotKey] = candidate.ID
				}
				return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true})
			}
		}
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		Handled:      true,
		Reject:       true,
		RejectCode:   "auth_unavailable",
		RejectReason: "all concurrency-limited credentials are busy",
	})
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
	// If some slots for this auth were swept as leaks, assume this usage record
	// belongs to one of those leaked slots and leave the live stamps untouched.
	// Otherwise pop the oldest live stamp (FIFO) as before.
	if inflight.swept[authID] > 0 {
		inflight.swept[authID]--
		if inflight.swept[authID] <= 0 {
			delete(inflight.swept, authID)
		}
		return okEnvelope(struct{}{})
	}
	stamps := inflight.slots[authID]
	if len(stamps) <= 1 {
		delete(inflight.slots, authID)
	} else {
		inflight.slots[authID] = stamps[1:]
	}
	return okEnvelope(struct{}{})
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return pluginConfig{slotTTL: 30 * time.Minute}
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
