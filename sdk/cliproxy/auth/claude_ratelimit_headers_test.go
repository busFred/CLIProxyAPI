package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestFilterClaudeUnifiedRateLimitHeaders_PrefixAllowlistOnly(t *testing.T) {
	src := http.Header{}
	src.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	src.Set("anthropic-ratelimit-unified-5h-reset", "1790000000")
	src["anthropic-ratelimit-unified-representative-claim"] = []string{"five_hour"}
	src.Set("Anthropic-Ratelimit-Requests-Remaining", "0")
	src.Set("Retry-After", "60")
	src.Set("Request-Id", "req_123")
	src.Set("Set-Cookie", "secret=1")
	src.Set("Anthropic-Ratelimit-Unified-Bad", "line\r\nInjected: 1")

	got := FilterClaudeUnifiedRateLimitHeaders(src)
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-Status":               "rejected",
		"Anthropic-Ratelimit-Unified-5h-Reset":             "1790000000",
		"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
	}
	if len(got) != len(want) {
		t.Fatalf("filtered headers = %v, want exactly %v", got, want)
	}
	for key, value := range want {
		if got.Get(key) != value {
			t.Fatalf("%s = %q, want %q", key, got.Get(key), value)
		}
	}
	if FilterClaudeUnifiedRateLimitHeaders(http.Header{"Retry-After": {"1"}}) != nil {
		t.Fatal("headers without the unified prefix must filter to nil")
	}
	if FilterClaudeUnifiedRateLimitHeaders(nil) != nil {
		t.Fatal("nil input must filter to nil")
	}
}

type upstreamHeaderErr struct{ headers http.Header }

func (e upstreamHeaderErr) Error() string                       { return "upstream 429" }
func (e upstreamHeaderErr) ClaudeRateLimitHeaders() http.Header { return e.headers }

func TestClaudeRateLimitResponseHeaders_PrefersUpstreamAndWrapped(t *testing.T) {
	upstream := upstreamHeaderErr{headers: http.Header{
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		"Retry-After":                        {"1"},
	}}
	got := ClaudeRateLimitResponseHeaders(markUpstreamExecutionAttempt(upstream))
	if got.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" || got.Get("Retry-After") != "" {
		t.Fatalf("headers = %v, want only the unified status", got)
	}
	if ClaudeRateLimitResponseHeaders(errors.New("boom")) != nil || ClaudeRateLimitResponseHeaders(nil) != nil {
		t.Fatal("errors without rate-limit data must yield nil")
	}
}

func claudeCooldownAuth(id string, next time.Time, signals map[string]string) *Auth {
	const model = "claude-fable-5-1"
	now := time.Now()
	auth := &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
					Signals:       signals,
				},
			},
		},
	}
	updateAggregatedAvailability(auth, now)
	return auth
}

func executeIntoClaudeCooldown(t *testing.T, provider string, auths ...*Auth) error {
	t.Helper()
	const model = "claude-fable-5-1"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&aliasRoutingExecutor{id: provider})
	for _, auth := range auths {
		auth.Provider = provider
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register: %v", err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		manager.RefreshSchedulerEntry(auth.ID)
	}
	_, err := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("Execute() error = nil, want cooldown error")
	}
	var cooldown *modelCooldownError
	if !errors.As(err, &cooldown) {
		t.Fatalf("Execute() error = %T (%v), want *modelCooldownError", err, err)
	}
	return err
}

func assertUnifiedReset(t *testing.T, headers http.Header, next time.Time) {
	t.Helper()
	reset, err := strconv.ParseInt(headers.Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64)
	if err != nil {
		t.Fatalf("reset %q is not Unix seconds: %v", headers.Get("Anthropic-Ratelimit-Unified-Reset"), err)
	}
	if reset < next.Unix() || reset > next.Unix()+1 {
		t.Fatalf("reset = %d, want %d", reset, next.Unix())
	}
}

func TestModelCooldown_ClaudeQuotaEmitsUnifiedHeaders(t *testing.T) {
	next := time.Now().Add(104 * time.Minute)
	err := executeIntoClaudeCooldown(t, "claude", claudeCooldownAuth("claude-cooldown-single", next, map[string]string{
		"Anthropic-Ratelimit-Unified-Status":               "rejected",
		"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day",
	}))

	headers := ClaudeRateLimitResponseHeaders(err)
	if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("status = %q, want rejected", got)
	}
	if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "seven_day" {
		t.Fatalf("claim = %q, want seven_day (preserved from the upstream 429)", got)
	}
	assertUnifiedReset(t, headers, next)
	if len(headers) != 3 {
		t.Fatalf("headers = %v, want exactly status, reset and claim", headers)
	}

	safe := SafeResponseHeaders(err)
	if safe.Get("Retry-After") == "" {
		t.Fatal("Retry-After must be kept")
	}
	for key := range safe {
		if strings.HasPrefix(strings.ToLower(key), "anthropic-") {
			t.Fatalf("SafeResponseHeaders leaked %s to non-Claude endpoints", key)
		}
	}
}

func TestModelCooldown_ClaudeEarliestCandidateDefinesResetAndClaim(t *testing.T) {
	soon := time.Now().Add(40 * time.Minute)
	later := time.Now().Add(3 * 24 * time.Hour)
	err := executeIntoClaudeCooldown(t, "claude",
		claudeCooldownAuth("claude-cooldown-later", later, map[string]string{
			"Anthropic-Ratelimit-Unified-Status":               "rejected",
			"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day",
		}),
		claudeCooldownAuth("claude-cooldown-soon", soon, map[string]string{
			"Anthropic-Ratelimit-Unified-Status":               "rejected",
			"Anthropic-Ratelimit-Unified-5h-Status":            "rejected",
			"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
		}),
	)
	headers := ClaudeRateLimitResponseHeaders(err)
	assertUnifiedReset(t, headers, soon)
	if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "five_hour" {
		t.Fatalf("claim = %q, want five_hour from the earliest-recovering credential", got)
	}
}

func TestModelCooldown_ClaudeWithoutEvidenceFallsBackByDuration(t *testing.T) {
	for name, tc := range map[string]struct {
		in    time.Duration
		claim string
	}{
		"session-sized":      {in: 90 * time.Minute, claim: "five_hour"},
		"longer-than-5h":     {in: 30 * time.Hour, claim: "seven_day"},
		"short-backoff-only": {in: 30 * time.Second, claim: ""},
	} {
		t.Run(name, func(t *testing.T) {
			next := time.Now().Add(tc.in)
			err := executeIntoClaudeCooldown(t, "claude", claudeCooldownAuth("claude-cooldown-"+name, next, nil))
			headers := ClaudeRateLimitResponseHeaders(err)
			if tc.claim == "" {
				if headers != nil {
					t.Fatalf("headers = %v, want none for a short cooldown without upstream evidence", headers)
				}
				return
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != tc.claim {
				t.Fatalf("claim = %q, want %q", got, tc.claim)
			}
			assertUnifiedReset(t, headers, next)
		})
	}
}

func TestModelCooldown_ClaimIgnoresNonRejectedAndOverageSignals(t *testing.T) {
	next := time.Now().Add(2 * time.Hour)
	for name, signals := range map[string]map[string]string{
		"allowed-status-claim": {
			"Anthropic-Ratelimit-Unified-Status":               "allowed",
			"Anthropic-Ratelimit-Unified-Representative-Claim": "seven_day",
		},
		"overage-claim": {
			"Anthropic-Ratelimit-Unified-Status":               "rejected",
			"Anthropic-Ratelimit-Unified-Representative-Claim": "overage",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := executeIntoClaudeCooldown(t, "claude", claudeCooldownAuth("claude-cooldown-"+name, next, signals))
			headers := ClaudeRateLimitResponseHeaders(err)
			if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "five_hour" {
				t.Fatalf("claim = %q, want duration fallback five_hour", got)
			}
		})
	}
}

func TestModelCooldown_NonClaudeAndPlainConstructorEmitNoUnifiedHeaders(t *testing.T) {
	next := time.Now().Add(2 * time.Hour)
	err := executeIntoClaudeCooldown(t, "codex", claudeCooldownAuth("codex-cooldown", next, map[string]string{
		"Anthropic-Ratelimit-Unified-Status": "rejected",
	}))
	if headers := ClaudeRateLimitResponseHeaders(err); headers != nil {
		t.Fatalf("codex cooldown headers = %v, want none", headers)
	}
	if headers := ClaudeRateLimitResponseHeaders(NewModelCooldownError("claude-fable-5-1", "claude", time.Hour)); headers != nil {
		t.Fatalf("plain cooldown headers = %v, want none without candidate evidence", headers)
	}
}

func TestModelCooldown_TransientUnavailableEmitsNoUnifiedHeaders(t *testing.T) {
	err := newAuthUnavailableErrorWithCause(time.Now().Add(2*time.Hour), time.Now(), nil)
	if headers := ClaudeRateLimitResponseHeaders(err); headers != nil {
		t.Fatalf("auth_unavailable headers = %v, want none", headers)
	}
}

func TestModelCooldown_ClaudeHintOnEveryCooldownConstructionPath(t *testing.T) {
	const model = "claude-fable-5-1"
	next := time.Now().Add(2 * time.Hour)
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-Status":               "rejected",
		"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
	}

	t.Run("route-model-availability", func(t *testing.T) {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		auth := claudeCooldownAuth("claude-cooldown-route", next, signals)
		_, err := manager.availableAuthsForRouteModel([]*Auth{auth}, "claude", model, time.Now())
		headers := ClaudeRateLimitResponseHeaders(err)
		if headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim") != "five_hour" {
			t.Fatalf("headers = %v, err = %v", headers, err)
		}
		assertUnifiedReset(t, headers, next)
	})

	t.Run("mixed-provider-scheduler", func(t *testing.T) {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.RegisterExecutor(&aliasRoutingExecutor{id: "claude"})
		manager.RegisterExecutor(&aliasRoutingExecutor{id: "claude-alt"})
		auth := claudeCooldownAuth("claude-cooldown-mixed", next, signals)
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		manager.RefreshSchedulerEntry(auth.ID)
		_, err := manager.Execute(context.Background(), []string{"claude", "claude-alt"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		headers := ClaudeRateLimitResponseHeaders(err)
		if headers.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
			t.Fatalf("headers = %v, err = %v", headers, err)
		}
		assertUnifiedReset(t, headers, next)
	})

	t.Run("restored-model-keeps-hint", func(t *testing.T) {
		original := newModelCooldownErrorWithCause("", "claude", time.Hour, nil)
		original.withClaudeRateLimitHint([]cooldownCandidate{{auth: claudeCooldownAuth("claude-cooldown-restore", next, signals), next: next}}, model, time.Now())
		restored := restoreModelCooldownErrorModel(original, model)
		if ClaudeRateLimitResponseHeaders(restored).Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
			t.Fatal("restoring the requested model must keep the Claude hint")
		}
	})
}

func TestClaudeCooldownMerge_UsesEarliestActualRecoveryInAllHeaders(t *testing.T) {
	const model = "claude-fable-5-1"
	now := time.Now()
	// The first proxy cooldown ends sooner, but its weekly window makes the
	// second account the first one that can actually serve this model.
	auths := []*Auth{
		claudeCooldownAuth(t.Name()+"-weekly", now.Add(10*time.Minute), windowSignals(now, map[string][3]string{
			"7d": {"rejected", "1", "3h"},
		})),
		claudeCooldownAuth(t.Name()+"-session", now.Add(40*time.Minute), windowSignals(now, map[string][3]string{
			"5h": {"rejected", "1", "1h"},
		})),
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	for _, path := range []string{"selector", "route", "scheduler"} {
		t.Run(path, func(t *testing.T) {
			var err error
			switch path {
			case "selector":
				_, err = getAvailableAuths(auths, "claude", model, now)
			case "route":
				_, err = manager.availableAuthsForRouteModel(auths, "claude", model, now)
			case "scheduler":
				err = executeIntoClaudeCooldown(t, "claude", auths...)
			}
			var cooldown *modelCooldownError
			if !errors.As(err, &cooldown) {
				t.Fatalf("error = %v, want model cooldown", err)
			}
			for _, headers := range []http.Header{cooldown.Headers(), ClaudeRateLimitResponseHeaders(err)} {
				assertUnifiedReset(t, headers, now.Add(time.Hour))
				if got := headers.Get(claudeUnifiedClaimHeader); got != claudeClaimFiveHour {
					t.Fatalf("claim = %q, want five_hour from earliest actual recovery", got)
				}
			}
			full := cooldown.Headers()
			if got := full.Get("Anthropic-Ratelimit-Unified-5h-Status"); got != "rejected" {
				t.Fatalf("5h status = %q, want winning account's window", got)
			}
			if full.Get("Anthropic-Ratelimit-Unified-5h-Reset") != full.Get(claudeUnifiedResetHeader) {
				t.Fatalf("window and unified reset disagree: %v", full)
			}
			seconds, errParse := strconv.ParseInt(full.Get("Retry-After"), 10, 64)
			if errParse != nil || seconds < 3595 || seconds > 3601 {
				t.Fatalf("Retry-After = %q, want about 1h", full.Get("Retry-After"))
			}
		})
	}
}

func TestClaudeCooldownMerge_ExclusionsApplyToClaudeResponseHeaders(t *testing.T) {
	const model = "claude-fable-5-1"
	now := time.Now()
	for _, reason := range []string{"quota", "cloudflare challenge"} {
		t.Run(reason, func(t *testing.T) {
			auth := claudeCooldownAuth(t.Name(), now.Add(10*time.Minute), map[string]string{
				claudeUnifiedStatusHeader: "rejected",
				claudeUnifiedClaimHeader:  "seven_day",
			})
			quota := &auth.ModelStates[model].Quota
			quota.Reason = reason
			// An evaluated RPM cooldown must not borrow an earlier quota observation.
			quota.cooldownEvaluated = true
			err := executeIntoClaudeCooldown(t, "claude", auth)
			if got := ClaudeRateLimitResponseHeaders(err); len(got) != 0 {
				t.Fatalf("excluded cooldown leaked Claude headers: %v", got)
			}
		})
	}
}

func TestClaudeCooldownMerge_CredentialSignalsDoNotApplyOtherModelsWeeklyWindow(t *testing.T) {
	now := time.Now()
	const model = "claude-opus-4-8"
	auth := hintAuth(t.Name(), model, windowSignals(now, map[string][3]string{
		"5h": {"allowed", "0.2", "1h"},
	}), true)
	signals := windowSignals(now, map[string][3]string{"7d_oi": {"rejected", "1", "72h"}})
	auth.Quota = QuotaState{
		Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
		Signals: signals, cooldownSignals: signals, cooldownEvaluated: true,
	}
	hint := claudeRateLimitHintFor([]cooldownCandidate{{auth: auth, next: now.Add(time.Hour)}}, model, time.Hour, now)
	if hint == nil || !hint.resetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("hint = %+v, want 1h cooldown without another model's 72h weekly window", hint)
	}
}
