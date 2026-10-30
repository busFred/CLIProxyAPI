package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func unifiedRejected(reset time.Time, claim string) http.Header {
	return http.Header{
		"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":                {strconv.FormatInt(reset.Unix(), 10)},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {claim},
		"Anthropic-Ratelimit-Unified-5h-Utilization":       {"1.0"},
	}
}

func TestAdjustClaudeUnifiedHeaders(t *testing.T) {
	now := time.Now()
	hint := &claudeRateLimitHint{claim: claudeClaimFiveHour, resetAt: now.Add(2 * time.Hour)}

	t.Run("cooldown earlier wins including claim", func(t *testing.T) {
		got, ok := adjustClaudeUnifiedHeaders(unifiedRejected(now.Add(4*time.Hour), "seven_day"), hint)
		if !ok {
			t.Fatal("want adjustment")
		}
		assertUnifiedReset(t, got, hint.resetAt)
		if got.Get("Anthropic-Ratelimit-Unified-Representative-Claim") != claudeClaimFiveHour {
			t.Fatalf("claim = %q", got.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
		}
		if got.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" || got.Get("Anthropic-Ratelimit-Unified-5h-Utilization") != "1.0" {
			t.Fatalf("other headers not preserved: %v", got)
		}
	})
	t.Run("upstream earlier is replaced by the hint", func(t *testing.T) {
		got, ok := adjustClaudeUnifiedHeaders(unifiedRejected(now.Add(time.Hour), "seven_day"), hint)
		if !ok {
			t.Fatal("want adjustment")
		}
		assertUnifiedReset(t, got, hint.resetAt)
		if got.Get("Anthropic-Ratelimit-Unified-Representative-Claim") != claudeClaimFiveHour {
			t.Fatalf("claim = %q", got.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
		}
	})
	t.Run("identical reset and claim is unchanged", func(t *testing.T) {
		if _, ok := adjustClaudeUnifiedHeaders(unifiedRejected(time.Unix(claudeResetUnix(hint.resetAt), 0), claudeClaimFiveHour), hint); ok {
			t.Fatal("identical values need no adjustment")
		}
	})
	t.Run("no hint", func(t *testing.T) {
		if _, ok := adjustClaudeUnifiedHeaders(unifiedRejected(now.Add(4*time.Hour), "seven_day"), nil); ok {
			t.Fatal("nil hint must not adjust")
		}
	})
	t.Run("no upstream reset or not rejected", func(t *testing.T) {
		noReset := unifiedRejected(now.Add(4*time.Hour), "seven_day")
		noReset.Del("Anthropic-Ratelimit-Unified-Reset")
		if _, ok := adjustClaudeUnifiedHeaders(noReset, hint); ok {
			t.Fatal("missing reset must not adjust")
		}
		allowed := unifiedRejected(now.Add(4*time.Hour), "seven_day")
		allowed.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		if _, ok := adjustClaudeUnifiedHeaders(allowed, hint); ok {
			t.Fatal("non-rejected status must not adjust")
		}
	})
}

type claudeTestRateLimitErr struct {
	headers    http.Header
	retryAfter time.Duration
}

func (e claudeTestRateLimitErr) Error() string                       { return "rate limited" }
func (e claudeTestRateLimitErr) StatusCode() int                     { return http.StatusTooManyRequests }
func (e claudeTestRateLimitErr) IsCredentialScoped() bool            { return true }
func (e claudeTestRateLimitErr) RetryAfter() *time.Duration          { return &e.retryAfter }
func (e claudeTestRateLimitErr) ClaudeRateLimitHeaders() http.Header { return e.headers.Clone() }

// claudeLimitExecutor fails every attempt with a 429; the first attempted
// credential resets in 2h and the second in 4h, so the last error carries the
// later reset.
type claudeLimitExecutor struct {
	id    string
	mu    sync.Mutex
	calls int
}

func (e *claudeLimitExecutor) Identifier() string { return e.id }

func (e *claudeLimitExecutor) next() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	d := 2 * time.Hour
	claim := "five_hour"
	if e.calls%2 == 0 {
		d = 4 * time.Hour
		claim = "seven_day"
	}
	return claudeTestRateLimitErr{headers: unifiedRejected(time.Now().Add(d), claim), retryAfter: d}
}

func (e *claudeLimitExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.next()
}

func (e *claudeLimitExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.next()
}

func (e *claudeLimitExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *claudeLimitExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.next()
}

func (e *claudeLimitExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newClaudeLimitManager(t *testing.T, providers ...string) (*Manager, string) {
	t.Helper()
	const model = "claude-fable-5-1"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(&claudeLimitExecutor{id: "claude"})
	for i, provider := range providers {
		auth := &Auth{ID: "adjust-" + strconv.Itoa(i) + "-" + t.Name(), Provider: provider, Status: StatusActive}
		if provider != "claude" {
			manager.RegisterExecutor(&claudeLimitExecutor{id: provider})
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register: %v", err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		manager.RefreshSchedulerEntry(auth.ID)
	}
	return manager, model
}

func assertWithin(t *testing.T, label string, got, want time.Time) {
	t.Helper()
	if d := got.Sub(want); d < -3*time.Second || d > 3*time.Second {
		t.Fatalf("%s = %v, want ~%v", label, got, want)
	}
}

func resetFromHeaders(t *testing.T, headers http.Header) time.Time {
	t.Helper()
	reset, err := strconv.ParseInt(headers.Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64)
	if err != nil {
		t.Fatalf("reset header %q: %v", headers.Get("Anthropic-Ratelimit-Unified-Reset"), err)
	}
	return time.Unix(reset, 0)
}

func checkAdjustedClaudeError(t *testing.T, manager *Manager, model string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want error")
	}
	if statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", statusCodeFromError(err))
	}
	var upstream claudeTestRateLimitErr
	if !errors.As(err, &upstream) {
		t.Fatalf("upstream error not reachable via errors.As: %T", err)
	}
	now := time.Now()
	headers := ClaudeRateLimitResponseHeaders(err)
	assertWithin(t, "reset", resetFromHeaders(t, headers), now.Add(2*time.Hour))
	if headers.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
		t.Fatalf("status header = %q", headers.Get("Anthropic-Ratelimit-Unified-Status"))
	}
	if headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim") != "five_hour" {
		t.Fatalf("claim = %q, want five_hour (the winning account's)", headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	}
	retryAfter, convErr := strconv.Atoi(SafeResponseHeaders(err).Get("Retry-After"))
	if convErr != nil || retryAfter < 7190 || retryAfter > 7205 {
		t.Fatalf("Retry-After = %q, want ~7200", SafeResponseHeaders(err).Get("Retry-After"))
	}
	if !HasTrustedRetryDeadline(err) {
		t.Fatal("adjusted error must keep a trusted retry deadline")
	}

	// A follow-up request now gets the model cooldown with the same reset.
	_, errNext := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	var cooldown *modelCooldownError
	if !errors.As(errNext, &cooldown) {
		t.Fatalf("follow-up error = %T (%v), want model cooldown", errNext, errNext)
	}
	assertWithin(t, "follow-up reset", resetFromHeaders(t, ClaudeRateLimitResponseHeaders(errNext)), now.Add(2*time.Hour))
}

func TestExecuteExhaustedClaudeReportsEarliestRecovery(t *testing.T) {
	manager, model := newClaudeLimitManager(t, "claude", "claude")
	_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	checkAdjustedClaudeError(t, manager, model, err)
}

func TestExecuteStreamExhaustedClaudeReportsEarliestRecovery(t *testing.T) {
	manager, model := newClaudeLimitManager(t, "claude", "claude")
	result, err := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if result != nil {
		t.Fatalf("result = %v, want nil", result)
	}
	checkAdjustedClaudeError(t, manager, model, err)
}

func TestExecuteExhaustedClaudeKeepsUpstreamWhenAnotherAccountIsAvailable(t *testing.T) {
	manager, model := newClaudeLimitManager(t, "claude", "claude", "claude")
	// Cap attempts at two of the three credentials: the third stays available.
	manager.SetRetryConfig(0, 0, 2)
	_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("want error")
	}
	var adjusted *claudeCooldownAdjustedError
	if errors.As(err, &adjusted) {
		t.Fatalf("error adjusted although a credential is still available: %v", err)
	}
}

func TestExecuteExhaustedMixedProvidersKeepsUpstream(t *testing.T) {
	manager, model := newClaudeLimitManager(t, "claude", "codex")
	_, err := manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("want error")
	}
	var adjusted *claudeCooldownAdjustedError
	if errors.As(err, &adjusted) {
		t.Fatalf("mixed providers must not be adjusted: %v", err)
	}
}

func TestExecuteCountTokensIsNotAdjusted(t *testing.T) {
	manager, model := newClaudeLimitManager(t, "claude", "claude")
	_, err := manager.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("want error")
	}
	var adjusted *claudeCooldownAdjustedError
	if errors.As(err, &adjusted) {
		t.Fatalf("count_tokens must not be adjusted: %v", err)
	}
}

func windowSignals(now time.Time, windows map[string][3]string) map[string]string {
	// windows: name -> {status, utilization, reset offset as duration string}
	signals := map[string]string{}
	for name, w := range windows {
		prefix := "Anthropic-Ratelimit-Unified-" + name
		if w[0] != "" {
			signals[prefix+"-Status"] = w[0]
		}
		if w[1] != "" {
			signals[prefix+"-Utilization"] = w[1]
		}
		if w[2] != "" {
			d, _ := time.ParseDuration(w[2])
			signals[prefix+"-Reset"] = strconv.FormatInt(now.Add(d).Unix(), 10)
		}
	}
	return signals
}

// hintAuth builds a Claude credential whose recorded signals sit on the model
// state (modelState) or only on the credential-level snapshot.
func hintAuth(id, model string, signals map[string]string, modelState bool) *Auth {
	auth := &Auth{ID: id, Provider: "claude", Status: StatusActive}
	if modelState {
		auth.ModelStates = map[string]*ModelState{model: {Quota: QuotaState{Signals: signals}}}
	} else {
		auth.Quota.Signals = signals
	}
	return auth
}

func TestClaudeRateLimitHint_AccountAvailableWhenEveryExhaustedWindowReset(t *testing.T) {
	now := time.Now()
	const model = "claude-fable-5-1"
	const opus = "claude-opus-4-8"
	day := 24 * time.Hour
	rej := "rejected"

	tests := []struct {
		name       string
		model      string
		auths      []*Auth
		nexts      []time.Duration
		wantAfter  time.Duration
		wantClaim  string
		wantNoHint bool
	}{
		{
			name:  "7d exhausted, 5h fine: 7d reset governs",
			model: model,
			auths: []*Auth{hintAuth("a", model, windowSignals(now, map[string][3]string{
				"7d": {rej, "1", "72h"}, "5h": {"allowed", "0.1", "1h"},
			}), true)},
			nexts:     []time.Duration{time.Hour},
			wantAfter: 3 * day, wantClaim: claudeClaimSevenDay,
		},
		{
			name:  "both exhausted: latest reset governs",
			model: model,
			auths: []*Auth{hintAuth("a", model, windowSignals(now, map[string][3]string{
				"7d": {rej, "1", "72h"}, "5h": {rej, "1", "2h"},
			}), true)},
			nexts:     []time.Duration{2 * time.Hour},
			wantAfter: 3 * day, wantClaim: claudeClaimSevenDay,
		},
		{
			name:  "utilization at 1 counts as exhausted without a status",
			model: model,
			auths: []*Auth{hintAuth("a", model, windowSignals(now, map[string][3]string{
				"7d": {"", "1.0", "72h"},
			}), true)},
			nexts:     []time.Duration{time.Hour},
			wantAfter: 3 * day, wantClaim: claudeClaimSevenDay,
		},
		{
			name:  "across accounts the earliest availability wins with its claim",
			model: model,
			auths: []*Auth{
				hintAuth("a", model, windowSignals(now, map[string][3]string{"7d": {rej, "1", "72h"}}), true),
				hintAuth("b", model, windowSignals(now, map[string][3]string{"5h": {rej, "1", "4h"}}), true),
			},
			nexts:     []time.Duration{time.Hour, 4 * time.Hour},
			wantAfter: 4 * time.Hour, wantClaim: claudeClaimFiveHour,
		},
		{
			name:  "model-scoped weekly window blocks its own model",
			model: model,
			auths: []*Auth{hintAuth("a", model, windowSignals(now, map[string][3]string{
				"7d_oi": {rej, "1", "72h"}, "7d": {"allowed", "0.2", "100h"}, "5h": {"allowed", "0.2", "1h"},
			}), true)},
			nexts:     []time.Duration{time.Hour},
			wantAfter: 3 * day, wantClaim: claudeClaimSevenDay,
		},
		{
			name:  "model-scoped weekly window on another model's state does not block this model",
			model: opus,
			auths: []*Auth{hintAuth("a", opus, windowSignals(now, map[string][3]string{
				"7d": {"allowed", "0.2", "100h"}, "5h": {"allowed", "0.2", "1h"},
			}), true)},
			nexts:     []time.Duration{time.Hour},
			wantAfter: time.Hour, wantClaim: claudeClaimFiveHour,
		},
		{
			name:  "credential-level snapshot ignores model-scoped weekly windows",
			model: opus,
			auths: []*Auth{hintAuth("a", opus, windowSignals(now, map[string][3]string{
				"7d_oi": {rej, "1", "72h"}, "7d": {"allowed", "0.2", "100h"}, "5h": {"allowed", "0.2", "1h"},
			}), false)},
			nexts:     []time.Duration{20 * time.Minute},
			wantAfter: 20 * time.Minute, wantClaim: claudeClaimFiveHour,
		},
		{
			name:  "credential-level snapshot still applies shared windows",
			model: opus,
			auths: []*Auth{hintAuth("a", opus, windowSignals(now, map[string][3]string{
				"7d_oi": {rej, "1", "200h"}, "7d": {rej, "1", "48h"},
			}), false)},
			nexts:     []time.Duration{20 * time.Minute},
			wantAfter: 2 * day, wantClaim: claudeClaimSevenDay,
		},
		{
			name:  "proxy cooldown later than the window reset wins",
			model: model,
			auths: []*Auth{hintAuth("a", model, windowSignals(now, map[string][3]string{
				"5h": {rej, "1", "1h"},
			}), true)},
			nexts:     []time.Duration{3 * time.Hour},
			wantAfter: 3 * time.Hour, wantClaim: claudeClaimFiveHour,
		},
		{
			name:      "no signals uses the cooldown alone",
			model:     model,
			auths:     []*Auth{hintAuth("a", model, nil, true)},
			nexts:     []time.Duration{6 * time.Hour},
			wantAfter: 6 * time.Hour, wantClaim: claudeClaimSevenDay,
		},
		{
			name:  "window reset already passed is ignored",
			model: model,
			auths: []*Auth{hintAuth("a", model, windowSignals(now, map[string][3]string{
				"7d": {rej, "1", "-1h"},
			}), true)},
			nexts: []time.Duration{2 * time.Hour},
			// The time ignores the stale window; the claim still falls back to
			// the recorded-header logic.
			wantAfter: 2 * time.Hour, wantClaim: claudeClaimSevenDay,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidates := make([]cooldownCandidate, len(tt.auths))
			var earliest time.Duration
			for i, auth := range tt.auths {
				candidates[i] = cooldownCandidate{auth: auth, next: now.Add(tt.nexts[i])}
				if i == 0 || tt.nexts[i] < earliest {
					earliest = tt.nexts[i]
				}
			}
			hint := claudeRateLimitHintFor(candidates, tt.model, earliest, now)
			if hint == nil {
				t.Fatal("hint = nil")
			}
			if d := hint.resetAt.Sub(now.Add(tt.wantAfter)); d < -time.Second || d > time.Second {
				t.Fatalf("resetAt = now+%v, want now+%v", hint.resetAt.Sub(now), tt.wantAfter)
			}
			if hint.claim != tt.wantClaim {
				t.Fatalf("claim = %q, want %q", hint.claim, tt.wantClaim)
			}
		})
	}
}

// The upstream 429 says 5h resets in 1h, but the same account's 7d window is
// exhausted for 3 days: the reported time must be the account's real
// availability, and with a second account at +4h, the earliest of the two.
func TestExhaustingRequestNeverReportsTimeBeforeAccountAvailability(t *testing.T) {
	now := time.Now()
	const model = "claude-fable-5-1"
	rej := "rejected"
	a := hintAuth("a", model, windowSignals(now, map[string][3]string{
		"5h": {rej, "1", "1h"}, "7d": {rej, "1", "72h"},
	}), true)
	upstream := unifiedRejected(now.Add(time.Hour), "five_hour")

	single := claudeRateLimitHintFor([]cooldownCandidate{{auth: a, next: now.Add(time.Hour)}}, model, time.Hour, now)
	got, ok := adjustClaudeUnifiedHeaders(upstream, single)
	if !ok {
		t.Fatal("want the later account availability to replace the early 5h reset")
	}
	assertWithin(t, "single account", resetFromHeaders(t, got), now.Add(72*time.Hour))
	if got.Get("Anthropic-Ratelimit-Unified-Representative-Claim") != claudeClaimSevenDay {
		t.Fatalf("claim = %q, want seven_day", got.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	}

	b := hintAuth("b", model, windowSignals(now, map[string][3]string{"5h": {rej, "1", "4h"}}), true)
	both := claudeRateLimitHintFor([]cooldownCandidate{
		{auth: a, next: now.Add(time.Hour)}, {auth: b, next: now.Add(4 * time.Hour)},
	}, model, time.Hour, now)
	got, ok = adjustClaudeUnifiedHeaders(upstream, both)
	if !ok {
		t.Fatal("want adjustment")
	}
	assertWithin(t, "two accounts", resetFromHeaders(t, got), now.Add(4*time.Hour))
	if got.Get("Anthropic-Ratelimit-Unified-Representative-Claim") != claudeClaimFiveHour {
		t.Fatalf("claim = %q, want five_hour", got.Get("Anthropic-Ratelimit-Unified-Representative-Claim"))
	}
}

// Account A is held back only by a rejected model-scoped weekly window (+3d)
// while its proxy cooldown is about to expire; B's 5h window resets at +4h.
// Retry-After must agree with the reported unified reset (+4h), not with the
// 1s proxy cooldown.
func TestModelCooldownRetryAfterFollowsClaudeHint(t *testing.T) {
	now := time.Now()
	const model = "claude-fable-5-1"
	rej := "rejected"
	a := hintAuth("a", model, windowSignals(now, map[string][3]string{"7d_oi": {rej, "1", "72h"}}), true)
	b := hintAuth("b", model, windowSignals(now, map[string][3]string{"5h": {rej, "1", "4h"}}), true)
	candidates := []cooldownCandidate{
		{auth: a, next: now.Add(time.Second)}, {auth: b, next: now.Add(4 * time.Hour)},
	}
	cooldownErr := newModelCooldownErrorWithCause(model, "", time.Second, nil).withClaudeRateLimitHint(candidates, model, now)

	assertWithin(t, "unified reset", resetFromHeaders(t, ClaudeRateLimitResponseHeaders(cooldownErr)), now.Add(4*time.Hour))
	retryAfter, err := strconv.Atoi(SafeResponseHeaders(cooldownErr).Get("Retry-After"))
	if err != nil || retryAfter < 14395 || retryAfter > 14401 {
		t.Fatalf("Retry-After = %q, want ~14400", SafeResponseHeaders(cooldownErr).Get("Retry-After"))
	}

	// Without a hint (non-Claude) the proxy cooldown still decides.
	plain := newModelCooldownErrorWithCause(model, "", time.Second, nil).withClaudeRateLimitHint(
		[]cooldownCandidate{{auth: &Auth{ID: "x", Provider: "codex"}, next: now.Add(time.Second)}}, model, now)
	if got := SafeResponseHeaders(plain).Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After without hint = %q, want 1", got)
	}
}
