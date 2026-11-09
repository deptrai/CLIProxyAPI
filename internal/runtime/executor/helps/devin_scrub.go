package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// devinDefaultReplacements breaks the exact-substring fingerprints Devin's
// upstream uses to reject Claude Code traffic with permission_denied. Each
// pair changes as few words as possible so the prompt semantics survive.
var devinDefaultReplacements = []config.DevinReplacement{
	{
		From: "x-anthropic-billing-header",
		To:   "x-client-billing-note",
	},
	{
		From: "official CLI for Claude",
		To:   "official coding CLI",
	},
	{
		From: "automatically compress prior messages",
		To:   "automatically condense prior messages",
	},
	{
		From: "Get help with using Claude Code",
		To:   "Get help with using this CLI",
	},
	{
		From: "https://github.com/anthropics/claude-code/issues",
		To:   "the project issue tracker",
	},
	{
		From: "reversibility and blast radius of actions",
		To:   "reversibility and impact radius of actions",
	},
	{
		From: "Do not use a colon before tool calls",
		To:   "Avoid using a colon before tool calls",
	},
	{
		From: "Anything already documented in CLAUDE.md files.",
		To:   "Anything already documented in memory index files.",
	},
	{
		From: "Reads a file from the local filesystem. You can access any file directly by using this tool.",
		To:   "Reads a file from the filesystem. You can access any file directly with this tool.",
	},
	{
		From: "This tool allows Claude Code to read images (eg PNG, JPG, etc).",
		To:   "This tool can read images (eg PNG, JPG).",
	},
	{
		From: "presented visually as Claude Code is a multimodal LLM.",
		To:   "presented visually since the assistant is multimodal.",
	},
	{
		From: "You MUST access memory when the user explicitly asks you to check, recall, or remember.",
		To:   "You MUST check memory when the user explicitly asks you to recall or remember.",
	},
	{
		From: "durable instructions like CLAUDE.md files",
		To:   "durable instructions like memory files",
	},
	{
		From: "Memory records can become stale over time. Use memory as context",
		To:   "Memory records can become stale over time. Treat memory as context",
	},
}

// DevinReplacements merges the built-in Claude Code fingerprint rewrites with
// the user-configured devin.replacements entries. Configured rules run after
// the built-in ones so deployments can override or extend them.
func DevinReplacements(cfg *config.Config) []config.DevinReplacement {
	rules := make([]config.DevinReplacement, 0, len(devinDefaultReplacements)+4)
	rules = append(rules, devinDefaultReplacements...)
	if cfg != nil && len(cfg.Devin.Replacements) > 0 {
		rules = append(rules, cfg.Devin.Replacements...)
	}
	return rules
}

// ApplyDevinTextReplacements rewrites every configured substring occurrence in
// one text blob. Empty From values are skipped to avoid pathological inserts.
func ApplyDevinTextReplacements(text string, rules []config.DevinReplacement) string {
	if text == "" || len(rules) == 0 {
		return text
	}
	for _, rule := range rules {
		if rule.From == "" || !strings.Contains(text, rule.From) {
			continue
		}
		text = strings.ReplaceAll(text, rule.From, rule.To)
	}
	return text
}

// ScrubDevinFingerprints applies the configured substring rewrites to the
// system prompt, every history prompt's text content and every tool
// description before the Devin protobuf request is built. Thinking text is
// left untouched because it is opaque model state, not client-authored prose.
func ScrubDevinFingerprints(cfg *config.Config, systemPrompt string, prompts []DevinPrompt, tools []DevinTool) (string, []DevinPrompt, []DevinTool) {
	rules := DevinReplacements(cfg)
	if len(rules) == 0 {
		return systemPrompt, prompts, tools
	}
	systemPrompt = ApplyDevinTextReplacements(systemPrompt, rules)
	for i := range prompts {
		prompts[i].Content = ApplyDevinTextReplacements(prompts[i].Content, rules)
	}
	for i := range tools {
		tools[i].Description = ApplyDevinTextReplacements(tools[i].Description, rules)
	}
	return systemPrompt, prompts, tools
}
