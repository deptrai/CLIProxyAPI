package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// devin429Trailer builds a Connect EOS trailer frame carrying a 429-style
// upstream error, mimicking the real Devin wire format.
func devin429Trailer(code, message string) []byte {
	trailer := `{"error":{"code":"` + code + `","message":"` + message + `"}}`
	return helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(trailer))
}

func TestDevinStreamTrailer429CarriesRetryAfter(t *testing.T) {
	t.Run("relative reset hint via consumeDevinFramesToInteractions", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(devin429Trailer("resource_exhausted",
			"Reached free model rate limit. Your limit will reset in 13 minutes (at 18:01 UTC)."))

		_, _, err := consumeDevinFramesToInteractions(&buf, "devin/swe-2", "swe-2-high")
		if err == nil {
			t.Fatal("expected trailer error, got nil")
		}
		se, ok := err.(interface {
			StatusCode() int
			RetryAfter() *time.Duration
			IsCredentialScoped() bool
		})
		if !ok {
			t.Fatalf("expected statusErr-compatible error, got %T", err)
		}
		if se.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("expected 429, got %d", se.StatusCode())
		}
		ra := se.RetryAfter()
		if ra == nil || *ra < 13*time.Minute-time.Minute || *ra > 13*time.Minute+time.Minute {
			t.Fatalf("expected ~13m retryAfter, got %v", ra)
		}
		if se.IsCredentialScoped() {
			t.Fatal("model rate limit must not be credential scoped")
		}
	})

	t.Run("usage quota message is credential scoped via consumeDevinFramesToInteractions", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(devin429Trailer("resource_exhausted",
			"Your weekly usage quota has been exhausted."))

		_, _, err := consumeDevinFramesToInteractions(&buf, "devin/swe-2", "swe-2-high")
		if err == nil {
			t.Fatal("expected trailer error, got nil")
		}
		se, ok := err.(interface {
			StatusCode() int
			RetryAfter() *time.Duration
			IsCredentialScoped() bool
		})
		if !ok {
			t.Fatalf("expected statusErr-compatible error, got %T", err)
		}
		if se.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("expected 429, got %d", se.StatusCode())
		}
		if ra := se.RetryAfter(); ra != nil {
			t.Fatalf("expected nil retryAfter for quota message without hint, got %v", *ra)
		}
		if !se.IsCredentialScoped() {
			t.Fatal("usage quota exhaustion must be credential scoped")
		}
	})

	t.Run("relative reset hint via streamDevinFrames", func(t *testing.T) {
		var buf bytes.Buffer
		buf.Write(devin429Trailer("resource_exhausted",
			"Reached free model rate limit. Your limit will reset in 13 minutes (at 18:01 UTC)."))

		e := &DevinExecutor{}
		out := make(chan cliproxyexecutor.StreamChunk, 10)
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}
		go func() {
			defer close(out)
			e.streamDevinFrames(
				context.Background(),
				&buf,
				cliproxyexecutor.Request{Model: "devin/swe-2"},
				opts,
				"swe-2-high",
				sdktranslator.FormatInteractions,
				nil,
				out,
			)
		}()

		var streamErr error
		for chunk := range out {
			if chunk.Err != nil {
				streamErr = chunk.Err
			}
		}
		if streamErr == nil {
			t.Fatal("expected stream error from 429 trailer, got nil")
		}
		se, ok := streamErr.(interface {
			StatusCode() int
			RetryAfter() *time.Duration
			IsCredentialScoped() bool
		})
		if !ok {
			t.Fatalf("expected statusErr-compatible error, got %T", streamErr)
		}
		if se.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("expected 429, got %d", se.StatusCode())
		}
		ra := se.RetryAfter()
		if ra == nil || *ra < 13*time.Minute-time.Minute || *ra > 13*time.Minute+time.Minute {
			t.Fatalf("expected ~13m retryAfter, got %v", ra)
		}
		if se.IsCredentialScoped() {
			t.Fatal("model rate limit must not be credential scoped")
		}
	})
}

func TestDevinResetHintFromMessageRelative(t *testing.T) {
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		msg  string
		want time.Duration
	}{
		{
			name: "minutes",
			msg:  "Reached free model rate limit. Your limit will reset in 13 minutes (at 18:01 UTC).",
			want: 13 * time.Minute,
		},
		{
			name: "hours and minutes",
			msg:  "Reached free model rate limit. Your limit will reset in 5 hours 30 minutes (at 23:30 UTC).",
			want: 5*time.Hour + 30*time.Minute,
		},
		{
			name: "seconds",
			msg:  "Reached free model rate limit. Your limit will reset in 52 seconds (at 18:01 UTC).",
			want: 52 * time.Second,
		},
		{
			name: "single hour",
			msg:  "Your limit will reset in 1 hour (at 19:00 UTC).",
			want: time.Hour,
		},
		{
			name: "no hint",
			msg:  "Your weekly usage quota has been exhausted.",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := devinResetHintFromMessage(tc.msg, now)
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("expected nil retry hint, got %v", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestDevinResetHintFromMessageAbsolute(t *testing.T) {
	now := time.Date(2026, 10, 8, 17, 55, 0, 0, time.UTC)
	got := devinResetHintFromMessage("reset at unknown (at 18:01 UTC)", now)
	if got == nil || *got != 6*time.Minute {
		t.Fatalf("expected 6m from absolute hint, got %v", got)
	}

	// Past absolute time rolls over to the next day.
	later := time.Date(2026, 10, 8, 18, 30, 0, 0, time.UTC)
	got = devinResetHintFromMessage("(at 18:01 UTC)", later)
	if got == nil || *got <= 23*time.Hour || *got > 24*time.Hour {
		t.Fatalf("expected ~24h rollover, got %v", got)
	}
}

func TestNewDevinStatusErrorRetryAfter(t *testing.T) {
	body := []byte(`devin upstream error (resource_exhausted): Reached free model rate limit. Your limit will reset in 13 minutes (at 18:01 UTC).`)
	err := newDevinStatusError(http.StatusTooManyRequests, http.Header{}, body)
	if err.retryAfter == nil || *err.retryAfter != 13*time.Minute {
		t.Fatalf("expected 13m retryAfter, got %v", err.retryAfter)
	}
	if err.credentialScoped {
		t.Fatal("model rate limit must not be credential scoped")
	}
}

func TestNewDevinStatusErrorCredentialScoped(t *testing.T) {
	body := []byte(`devin upstream error (failed_precondition): Your weekly usage quota has been exhausted.`)
	err := newDevinStatusError(http.StatusTooManyRequests, http.Header{}, body)
	if !err.credentialScoped {
		t.Fatal("usage quota exhaustion must be credential scoped")
	}
}

func TestNewDevinTrailerStatusError(t *testing.T) {
	err := newDevinTrailerStatusError(http.StatusTooManyRequests,
		errors.New("devin upstream error (resource_exhausted): Reached free model rate limit. Your limit will reset in 9 minutes (at 18:01 UTC)."))
	if err.retryAfter == nil || *err.retryAfter != 9*time.Minute {
		t.Fatalf("expected 9m retryAfter, got %v", err.retryAfter)
	}
}
