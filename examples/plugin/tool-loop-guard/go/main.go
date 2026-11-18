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
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var state = struct {
	mu     sync.Mutex
	config pluginConfig
}{config: defaultConfig()}

type pluginConfig struct {
	Models    []string `yaml:"models"`
	ToolName  string   `yaml:"tool_name"`
	Threshold int      `yaml:"threshold"`
}

func defaultConfig() pluginConfig {
	return pluginConfig{Models: []string{"grok-4.7*"}, ToolName: "Skill", Threshold: 3}
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
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor bool `json:"request_interceptor"`
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
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
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
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	default:
		return okEnvelope(struct{}{})
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"*"}
	}
	if strings.TrimSpace(cfg.ToolName) == "" {
		cfg.ToolName = "Skill"
	}
	if cfg.Threshold < 2 {
		cfg.Threshold = 3
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.config = cfg
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "tool-loop-guard",
			Version:          "0.1.0",
			Author:           "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeString, Description: "Wildcard model patterns to guard (default grok-4.7*)."},
				{Name: "tool_name", Type: pluginapi.ConfigFieldTypeString, Description: "Tool name to strip when looped (default Skill)."},
				{Name: "threshold", Type: pluginapi.ConfigFieldTypeInteger, Description: "Same-tool call count that triggers stripping (default 3)."},
			},
		},
		Capabilities: registrationCapability{RequestInterceptor: true},
	}
}

func interceptAfterAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	// Passthrough responses must omit Body: the host treats any non-empty Body
	// as a replacement and would clobber opts.OriginalRequest.
	model := req.RequestedModel
	if model == "" {
		model = req.Model
	}
	if !modelMatches(cfg.Models, model) || len(req.Body) == 0 {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(req.Body, &payload); errUnmarshal != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	if !loopDetected(payload, cfg.ToolName, cfg.Threshold) {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	// Strip the looping tool from tools[] and append a closing note.
	if tools, ok := payload["tools"].([]any); ok {
		filtered := tools[:0]
		for _, t := range tools {
			if toolName(t) == cfg.ToolName {
				continue
			}
			filtered = append(filtered, t)
		}
		if len(filtered) == 0 {
			// An explicit empty tools array trips strict upstreams; dropping
			// the key signals "no tools available" instead.
			delete(payload, "tools")
		} else {
			payload["tools"] = filtered
		}
	}
	scrubToolChoice(payload, cfg.ToolName)
	msgs, _ := payload["messages"].([]any)
	payload["messages"] = append(msgs, map[string]any{
		"role":    "user",
		"content": fmt.Sprintf("[Runtime instruction] The %s tool has been removed because it was invoked repeatedly for the same target. The skill's instructions are already loaded in context — proceed with the task now.", cfg.ToolName),
	})

	newBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: newBody})
}

// scrubToolChoice neutralizes a tool_choice that forces the removed tool (or
// any tool at all) after the tool was stripped from tools[] — otherwise the
// upstream errors on a tool_choice pointing at an undefined tool.
func scrubToolChoice(payload map[string]any, toolName string) {
	tc, ok := payload["tool_choice"]
	if !ok || tc == nil {
		return
	}
	switch v := tc.(type) {
	case string:
		// OpenAI wire: "auto"/"required"/"none" or a bare tool name.
		if strings.EqualFold(v, "required") || strings.EqualFold(v, toolName) {
			payload["tool_choice"] = "auto"
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		name, _ := v["name"].(string)
		if fn, _ := v["function"].(map[string]any); fn != nil {
			if n, _ := fn["name"].(string); n != "" {
				name = n
			}
		}
		forced := strings.EqualFold(name, toolName) || typ == "required" || typ == "any"
		if !forced {
			return
		}
		// Anthropic wire ("tool"/"any" types) needs an object; OpenAI accepts
		// the bare "auto" string.
		if typ == "tool" || typ == "any" {
			payload["tool_choice"] = map[string]any{"type": "auto"}
		} else {
			payload["tool_choice"] = "auto"
		}
	}
}

func modelMatches(patterns []string, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || p == "*" {
			return true
		}
		if strings.HasPrefix(p, "*") && strings.HasSuffix(p, "*") && len(p) > 2 {
			if strings.Contains(model, p[1:len(p)-1]) {
				return true
			}
			continue
		}
		if strings.HasPrefix(p, "*") {
			if strings.HasSuffix(model, p[1:]) {
				return true
			}
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(model, p[:len(p)-1]) {
				return true
			}
			continue
		}
		if model == p {
			return true
		}
	}
	return false
}

func toolName(t any) string {
	m, ok := t.(map[string]any)
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok {
			return n
		}
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}

func loopDetected(payload map[string]any, toolName string, threshold int) bool {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	counts := map[string]int{}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI wire: assistant messages carry tool_calls[].function with
		// arguments as a JSON string.
		calls, _ := msg["tool_calls"].([]any)
		for _, c := range calls {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := cm["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name != toolName {
				continue
			}
			args, _ := fn["arguments"].(string)
			var argMap map[string]any
			if json.Unmarshal([]byte(args), &argMap) == nil {
				if s, ok := argMap["skill"].(string); ok && s != "" {
					counts[s]++
					continue
				}
				if s, ok := argMap["name"].(string); ok && s != "" {
					counts[s]++
					continue
				}
			}
			counts["__unknown__"]++
		}
		// Anthropic wire: tool invocations are content blocks of type
		// "tool_use" with input as a decoded object.
		content, _ := msg["content"].([]any)
		for _, block := range content {
			bm, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := bm["type"].(string); t != "tool_use" {
				continue
			}
			if name, _ := bm["name"].(string); name != toolName {
				continue
			}
			input, _ := bm["input"].(map[string]any)
			if s, ok := input["skill"].(string); ok && s != "" {
				counts[s]++
				continue
			}
			if s, ok := input["name"].(string); ok && s != "" {
				counts[s]++
				continue
			}
			counts["__unknown__"]++
		}
	}
	for _, n := range counts {
		if n >= threshold {
			return true
		}
	}
	return false
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
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
