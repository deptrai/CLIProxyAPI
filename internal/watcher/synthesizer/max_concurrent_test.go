package synthesizer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The cred-concurrency plugin resolves per-auth limits from configured limits
// keyed by auth ID first, then from the candidate's max_concurrent
// metadata/attribute. Scheduler candidates only carry attributes, so the auth
// file's max_concurrent must be copied into attributes during synthesis.
// Otherwise a renamed auth file silently becomes unlimited when the configured
// limit still references the old filename.
func TestSynthesizeAuthFileExposesMaxConcurrentAttribute(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: authDir,
		Now:     time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC),
	}

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "number", raw: `{"type":"claude","email":"a@example.test","max_concurrent":5}`, want: "5"},
		{name: "string", raw: `{"type":"claude","email":"b@example.test","max_concurrent":"3"}`, want: "3"},
		{name: "legacy key", raw: `{"type":"claude","email":"c@example.test","max-concurrent":7}`, want: "7"},
		{name: "canonical wins", raw: `{"type":"claude","email":"d@example.test","max_concurrent":2,"max-concurrent":9}`, want: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auths := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "auth.json"), tc.raw)
			if len(auths) != 1 {
				t.Fatalf("len(auths) = %d, want 1", len(auths))
			}
			if got := auths[0].Attributes["max_concurrent"]; got != tc.want {
				t.Fatalf("max_concurrent attribute = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSynthesizeAuthFileOmitsMaxConcurrentWhenUnset(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: authDir,
		Now:     time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC),
	}

	auths := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "auth.json"), `{"type":"claude","email":"a@example.test"}`)
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	if got, ok := auths[0].Attributes["max_concurrent"]; ok {
		t.Fatalf("max_concurrent attribute = %q, want absent", got)
	}
}

// Non-positive or non-numeric values must not produce an attribute: the plugin
// treats a missing limit as unlimited anyway, but a stored "-1" would be
// silently clamped to 0 (unlimited) downstream, hiding a config mistake.
func TestSynthesizeAuthFileRejectsInvalidMaxConcurrent(t *testing.T) {
	authDir := t.TempDir()
	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: authDir,
		Now:     time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC),
	}

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "negative number", raw: `{"type":"claude","email":"a@example.test","max_concurrent":-1}`},
		{name: "negative string", raw: `{"type":"claude","email":"b@example.test","max_concurrent":"-2"}`},
		{name: "zero", raw: `{"type":"claude","email":"c@example.test","max_concurrent":0}`},
		{name: "non-numeric", raw: `{"type":"claude","email":"d@example.test","max_concurrent":"lots"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auths := mustSynthesizeAuthFile(t, ctx, filepath.Join(authDir, "auth.json"), tc.raw)
			if len(auths) != 1 {
				t.Fatalf("len(auths) = %d, want 1", len(auths))
			}
			if got, ok := auths[0].Attributes["max_concurrent"]; ok {
				t.Fatalf("max_concurrent attribute = %q, want absent", got)
			}
		})
	}
}
