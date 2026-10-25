package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type claudeRateLimitHeaderProvider interface {
	ClaudeRateLimitHeaders() http.Header
}

func newClaudeUnifiedRejectionServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1893456000")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", "1893456000")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("Request-Id", "req_secret")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func assertClaudeUnifiedHeadersOnly(t *testing.T, err error) {
	t.Helper()
	var provider claudeRateLimitHeaderProvider
	if !errors.As(err, &provider) || provider == nil {
		t.Fatalf("error %T does not expose ClaudeRateLimitHeaders()", err)
	}
	headers := provider.ClaudeRateLimitHeaders()
	if got := headers.Get("Anthropic-Ratelimit-Unified-Reset"); got != "1893456000" {
		t.Fatalf("unified reset = %q, want 1893456000", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "five_hour" {
		t.Fatalf("claim = %q, want five_hour", got)
	}
	if headers.Get("Retry-After") != "" || headers.Get("Request-Id") != "" || headers.Get("Content-Type") != "" {
		t.Fatalf("non-unified headers leaked: %v", headers)
	}
	if got := cliproxyauth.ClaudeRateLimitResponseHeaders(err).Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("response headers status = %q, want rejected", got)
	}
}

func TestClaudeExecutor_UpstreamUnified429CarriesOnlyUnifiedHeaders(t *testing.T) {
	server := newClaudeUnifiedRejectionServer(t)
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "claude-unified-headers",
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}

	_, err := executor.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("Execute() error = nil, want 429")
	}
	assertClaudeUnifiedHeadersOnly(t, err)

	opts.Stream = true
	_, err = executor.ExecuteStream(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("ExecuteStream() error = nil, want 429")
	}
	assertClaudeUnifiedHeadersOnly(t, err)
}
