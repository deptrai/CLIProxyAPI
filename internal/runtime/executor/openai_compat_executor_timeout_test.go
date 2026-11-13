package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDoRequestWithHeaderTimeoutDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := doRequestWithHeaderTimeout(context.Background(), server.Client(), req, 0)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDoRequestWithHeaderTimeoutFires(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	start := time.Now()
	_, err = doRequestWithHeaderTimeout(context.Background(), server.Client(), req, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	se, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr", err)
	}
	if se.StatusCode() != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", se.StatusCode())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, want ~50ms", elapsed)
	}
}

func TestDoRequestWithHeaderTimeoutParentCancelNotSynthesized(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, errDo := doRequestWithHeaderTimeout(ctx, server.Client(), req, 10*time.Second)
		done <- errDo
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	err = <-done
	if err == nil {
		t.Fatal("expected error after parent cancel")
	}
	if se, ok := err.(statusErr); ok && se.StatusCode() == http.StatusGatewayTimeout {
		t.Fatal("parent cancellation must not be reported as upstream timeout")
	}
}

func TestDoRequestWithHeaderTimeoutBodyOutlivesDeadline(t *testing.T) {
	// Headers arrive quickly but the body trickles past the header timeout;
	// the body must remain readable because the deadline only guards headers.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(120 * time.Millisecond)
		_, _ = w.Write([]byte("late-body"))
	}))
	defer server.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := doRequestWithHeaderTimeout(context.Background(), server.Client(), req, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "late-body" {
		t.Fatalf("body = %q, want %q", body, "late-body")
	}
}

func TestOpenAICompatTimeoutDuration(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset", "", 0},
		{"seconds", "45s", 45 * time.Second},
		{"minutes", "1m30s", 90 * time.Second},
		{"zero", "0s", 0},
		{"negative", "-5s", 0},
		{"invalid", "abc", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.OpenAICompatibility{Timeout: tc.raw}
			if got := c.TimeoutDuration(); got != tc.want {
				t.Fatalf("TimeoutDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	var nilCompat *config.OpenAICompatibility
	if got := nilCompat.TimeoutDuration(); got != 0 {
		t.Fatalf("nil TimeoutDuration = %v, want 0", got)
	}
}
