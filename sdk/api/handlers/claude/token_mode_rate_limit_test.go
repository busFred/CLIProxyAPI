package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// Go equivalent of the T3 Code auto-resume regex.
var t3ResumeRegexp = regexp.MustCompile(`^API Error: 503 \[[^\]\r\n]+\] \[429\]: \{"type":"error","error":\{"type":"rate_limit_error",[^\r\n]*\} \(reset after (?:(\d+)h ?)?(?:(\d+)m ?)?(?:(\d+)s)?\)`)

func TestFormatClaudeResetDuration(t *testing.T) {
	tests := map[int64]string{
		3723: "1h 2m 3s", 150: "2m 30s", 14400: "4h", 45: "45s", 3600: "1h", 3601: "1h 1s", 60: "1m", 7200 + 5: "2h 5s",
	}
	for secs, want := range tests {
		if got := formatClaudeResetDuration(secs); got != want {
			t.Errorf("formatClaudeResetDuration(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestIsClaudeCodeTokenMode(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		beta []string
		want bool
	}{
		{"cli no beta", "claude-cli/2.1.312 (external, cli)", nil, true},
		{"cli other beta", "claude-cli/2.1", []string{"claude-code-20250219,interleaved-thinking-2025-05-14"}, true},
		{"cli oauth beta", "claude-cli/2.1", []string{"claude-code-20250219,oauth-2025-04-20"}, false},
		{"cli oauth beta spaced upper", "claude-cli/2.1", []string{"claude-code-20250219, OAuth-2025-04-20 "}, false},
		{"cli oauth beta repeated header", "claude-cli/2.1", []string{"a", "b", "oauth-2025-04-20"}, false},
		{"cli similar beta", "claude-cli/2.1", []string{"oauth-2025-04-20-x"}, true},
		{"other agent", "curl/8", nil, false},
		{"no agent", "", nil, false},
		{"uppercase prefix", "Claude-CLI/2.1", nil, false},
		{"sdk agent", "Anthropic/JS 0.1", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.ua != "" {
				h.Set("User-Agent", tt.ua)
			}
			for _, b := range tt.beta {
				h.Add("anthropic-beta", b)
			}
			if got := isClaudeCodeTokenMode(h); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeResetSeconds(t *testing.T) {
	now := time.Unix(1000, 500_000_000)
	mk := func(status, reset string) http.Header {
		h := http.Header{}
		if status != "" {
			h.Set("Anthropic-Ratelimit-Unified-Status", status)
		}
		if reset != "" {
			h.Set("Anthropic-Ratelimit-Unified-Reset", reset)
		}
		return h
	}
	if got, ok := claudeResetSeconds(mk("rejected", "1010"), now); !ok || got != 10 {
		t.Fatalf("got %d %v, want 10 true (ceil of 9.5)", got, ok)
	}
	for name, h := range map[string]http.Header{
		"allowed":  mk("allowed", "1010"),
		"no reset": mk("rejected", ""),
		"garbage":  mk("rejected", "soon"),
		"past":     mk("rejected", "900"),
		"now":      mk("rejected", "1000"),
		"empty":    nil,
	} {
		if _, ok := claudeResetSeconds(h, now); ok {
			t.Errorf("%s: expected no reset", name)
		}
	}
}

func TestBuildClaudeTokenModeRateLimitBody(t *testing.T) {
	tests := []struct {
		name, model, msg, wantPrefix string
		secs                         int64
		wantGroups                   [3]string
	}{
		{"full", "claude-opus-5-5", "limit \"hit\"\r\nnow", "[claude/claude-opus-5-5] [429]: ", 3723, [3]string{"1", "2", "3"}},
		{"no model", "", "", "[claude] [429]: ", 150, [3]string{"", "2", "30"}},
		{"hostile model", "a]b\r\nc", "x", "[claude/abc] [429]: ", 45, [3]string{"", "", "45"}},
		{"hours only", "m", "x", "[claude/m] [429]: ", 14400, [3]string{"4", "", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := buildClaudeTokenModeRateLimitBody(tt.model, tt.msg, tt.secs)
			if err != nil {
				t.Fatal(err)
			}
			text := gjson.GetBytes(body, "error.message").String()
			if !strings.HasPrefix(text, tt.wantPrefix) {
				t.Fatalf("message %q lacks prefix %q", text, tt.wantPrefix)
			}
			m := t3ResumeRegexp.FindStringSubmatch("API Error: 503 " + text + ". This is a server-side issue")
			if m == nil {
				t.Fatalf("message does not match T3 regex: %q", text)
			}
			for i, want := range tt.wantGroups {
				if m[i+1] != want {
					t.Errorf("group %d = %q, want %q", i+1, m[i+1], want)
				}
			}
			inner := text[len(tt.wantPrefix):strings.LastIndex(text, " (reset after")]
			if !gjson.Valid(inner) || gjson.Get(inner, "error.type").String() != "rate_limit_error" {
				t.Fatalf("inner is not valid rate_limit JSON: %s", inner)
			}
			if got := gjson.Get(inner, "error.message").String(); strings.ContainsAny(got, "\r\n") || got == "" {
				t.Fatalf("inner message = %q", got)
			}
			if strings.Contains(string(body), "\n") {
				t.Fatalf("raw newline in body")
			}
		})
	}
	body, _ := buildClaudeTokenModeRateLimitBody("m", "  ", 5)
	if !strings.Contains(gjson.GetBytes(body, "error.message").String(), "exceed your account's rate limit") {
		t.Fatalf("default message missing: %s", body)
	}
}

func tokenModeRequest(ua string, beta ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	for _, b := range beta {
		r.Header.Add("Anthropic-Beta", b)
	}
	return r
}

func writeUpstream429(t *testing.T, req *http.Request, mark bool, status int, resetIn time.Duration) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	if mark {
		markClaudeMessagesRequest(c, "claude-opus-5-5")
	}
	upstream := http.Header{
		"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
	}
	if resetIn != 0 {
		upstream.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(time.Now().Add(resetIn).Unix(), 10))
	}
	(&ClaudeCodeAPIHandler{}).WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      claudeUnifiedHeaderErr{headers: upstream},
		Addon:      upstream,
	})
	return recorder, c
}

func assertTokenModeResponse(t *testing.T, recorder *httptest.ResponseRecorder, c *gin.Context) {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	h := recorder.Header()
	if h.Get("X-Should-Retry") != "false" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", h)
	}
	retry, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || retry <= 0 {
		t.Fatalf("Retry-After = %q", h.Get("Retry-After"))
	}
	if h.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" || h.Get("Anthropic-Ratelimit-Unified-Reset") == "" {
		t.Fatalf("unified headers dropped: %v", h)
	}
	text := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String()
	m := t3ResumeRegexp.FindStringSubmatch("API Error: 503 " + text)
	if m == nil || !strings.HasPrefix(text, "[claude/claude-opus-5-5] [429]: ") {
		t.Fatalf("message mismatch: %q", text)
	}
	total := 0
	for i, mult := range []int{3600, 60, 1} {
		if m[i+1] != "" {
			n, _ := strconv.Atoi(m[i+1])
			total += n * mult
		}
	}
	if total != retry {
		t.Fatalf("reset parsed from text = %ds, Retry-After = %d", total, retry)
	}
	if v, _ := c.Get(handlers.LogicalResponseStatusKey); v != http.StatusTooManyRequests {
		t.Fatalf("logical status = %v, want 429", v)
	}
}

func TestWriteErrorResponse_TokenModeForwarded429(t *testing.T) {
	recorder, c := writeUpstream429(t, tokenModeRequest("claude-cli/2.1.312 (external, cli)"), true, 429, 3723*time.Second)
	assertTokenModeResponse(t, recorder, c)
}

func TestWriteErrorResponse_TokenModeUnchangedCases(t *testing.T) {
	tests := []struct {
		name   string
		req    *http.Request
		mark   bool
		status int
		reset  time.Duration
	}{
		{"subscriber mode", tokenModeRequest("claude-cli/2.1", "claude-code-20250219,oauth-2025-04-20"), true, 429, time.Hour},
		{"other UA", tokenModeRequest("curl/8"), true, 429, time.Hour},
		{"not messages request", tokenModeRequest("claude-cli/2.1"), false, 429, time.Hour},
		{"no reset", tokenModeRequest("claude-cli/2.1"), true, 429, 0},
		{"reset in past", tokenModeRequest("claude-cli/2.1"), true, 429, -time.Minute},
		{"non-429", tokenModeRequest("claude-cli/2.1"), true, 500, time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, c := writeUpstream429(t, tt.req, tt.mark, tt.status, tt.reset)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			if recorder.Header().Get("X-Should-Retry") != "" {
				t.Fatalf("unexpected x-should-retry")
			}
			if recorder.Header().Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
				t.Fatalf("unified headers should still be forwarded: %v", recorder.Header())
			}
			if gjson.GetBytes(recorder.Body.Bytes(), "type").String() != "error" {
				t.Fatalf("body no longer Anthropic-shaped: %s", recorder.Body.String())
			}
			if _, ok := c.Get(handlers.LogicalResponseStatusKey); ok {
				t.Fatal("logical status must not be set")
			}
		})
	}
}

func TestWriteErrorResponse_TokenModeProxyCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "claude-fable-5-token-mode"
	next := time.Now().Add(104 * time.Minute)
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(cooldownTestExecutor{})
	auth := &coreauth.Auth{
		ID: "claude-token-mode-cooldown", Provider: "claude", Status: coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{
			model: {
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: next,
				Quota: coreauth.QuotaState{
					Exceeded: true, Reason: "quota", NextRecoverAt: next,
					Signals: map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected"},
				},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	manager.RefreshSchedulerEntry(auth.ID)
	_, err := manager.Execute(context.Background(), []string{"claude"}, coreexecutor.Request{Model: model}, coreexecutor.Options{})
	if err == nil {
		t.Fatal("want cooldown error")
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = tokenModeRequest("claude-cli/2.1.312")
	markClaudeMessagesRequest(c, "claude-opus-5-5")
	(&ClaudeCodeAPIHandler{}).WriteErrorResponse(c, handlers.ExecutionErrorMessage(err))
	assertTokenModeResponse(t, recorder, c)
}

func TestTokenModeRewriteKeepsRecordedStatus429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = tokenModeRequest("claude-cli/2.1")
	base := handlers.NewBaseAPIHandlers(nil, nil)
	ctx, cancel := base.GetContextWithCancel(nil, c, context.Background())
	markClaudeMessagesRequest(c, "claude-opus-5-5")
	upstream := http.Header{
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":  {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
	}
	(&ClaudeCodeAPIHandler{}).WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: 429, Error: claudeUnifiedHeaderErr{headers: upstream}, Addon: upstream,
	})
	cancel()
	if recorder.Code != 503 {
		t.Fatalf("wire status = %d", recorder.Code)
	}
	if got := logging.GetResponseStatus(ctx); got != 429 {
		t.Fatalf("recorded status = %d, want 429", got)
	}
}
