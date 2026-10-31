package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// claudeAccountLimit is the 429 one credential answers with: its 5h and 7d
// windows and the top-level reset and claim Anthropic chose for it.
type claudeAccountLimit struct {
	fiveHour, sevenDay [3]string // status, utilization, reset offset
	reset              time.Duration
	claim              string
}

func (l claudeAccountLimit) headers(now time.Time) http.Header {
	h := http.Header{}
	for k, v := range windowSignals(now, map[string][3]string{"5h": l.fiveHour, "7d": l.sevenDay}) {
		h.Set(k, v)
	}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(now.Add(l.reset).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", l.claim)
	return h
}

// claudeAccountsExecutor answers every request on a credential with that
// credential's 429, so the final error carries the last attempted account's.
type claudeAccountsExecutor struct {
	limits map[string]claudeAccountLimit
}

func (e *claudeAccountsExecutor) Identifier() string { return "claude" }

// fail publishes the response headers as the real executor does, so the
// manager records them as the credential's quota signals.
func (e *claudeAccountsExecutor) fail(ctx context.Context, auth *Auth) error {
	l := e.limits[auth.ID]
	headers := l.headers(time.Now())
	internallogging.SetResponseHeaders(ctx, headers)
	return claudeTestRateLimitErr{headers: headers, retryAfter: l.reset}
}

func (e *claudeAccountsExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.fail(ctx, auth)
}

func (e *claudeAccountsExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.fail(ctx, auth)
}

func (e *claudeAccountsExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *claudeAccountsExecutor) CountTokens(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.fail(ctx, auth)
}

func (e *claudeAccountsExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

// newClaudeAccountsManager registers one credential per limit, in order. With
// disableCooling the proxy records no cooldown, as with disable-cooling: true.
func newClaudeAccountsManager(t *testing.T, disableCooling bool, limits ...claudeAccountLimit) (*Manager, string) {
	t.Helper()
	const model = "claude-opus-5-5"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{DisableCooling: disableCooling})
	manager.SetRetryConfig(0, 0, 0)
	exec := &claudeAccountsExecutor{limits: map[string]claudeAccountLimit{}}
	manager.RegisterExecutor(exec)
	for i, limit := range limits {
		auth := &Auth{ID: "accounts-" + strconv.Itoa(i) + "-" + t.Name(), Provider: "claude", Status: StatusActive}
		exec.limits[auth.ID] = limit
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register: %v", err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		manager.RefreshSchedulerEntry(auth.ID)
	}
	return manager, model
}

var (
	// Session window drained, resets in 15 minutes.
	fiveHourSoon = claudeAccountLimit{
		fiveHour: [3]string{"rejected", "1.0", "15m"},
		sevenDay: [3]string{"allowed", "0.4", "72h"},
		reset:    15 * time.Minute,
		claim:    "five_hour",
	}
	// Session window drained, resets in 2.5 hours.
	fiveHourLater = claudeAccountLimit{
		fiveHour: [3]string{"rejected", "1.0", "150m"},
		sevenDay: [3]string{"allowed", "0.6", "96h"},
		reset:    150 * time.Minute,
		claim:    "five_hour",
	}
	// Weekly window drained; the session window is empty and resets soon, and
	// the top-level reset names that session reset.
	weeklyDrained = claudeAccountLimit{
		fiveHour: [3]string{"allowed", "0.0", "10m"},
		sevenDay: [3]string{"rejected", "1.0", "23h"},
		reset:    10 * time.Minute,
		claim:    "five_hour",
	}
)

func execClaudeAccounts(t *testing.T, manager *Manager, model string, stream bool) error {
	t.Helper()
	req := cliproxyexecutor.Request{Model: model}
	if stream {
		result, err := manager.ExecuteStream(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
		if result != nil {
			t.Fatalf("result = %v, want nil", result)
		}
		return err
	}
	_, err := manager.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	return err
}

func checkClaudeReset(t *testing.T, err error, want time.Duration, claim string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error")
	}
	headers := ClaudeRateLimitResponseHeaders(err)
	assertWithin(t, "reset", resetFromHeaders(t, headers), time.Now().Add(want))
	if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != claim {
		t.Fatalf("claim = %q, want %q", got, claim)
	}
	retryAfter, convErr := strconv.Atoi(SafeResponseHeaders(err).Get("Retry-After"))
	if wantSecs := int(want / time.Second); convErr != nil || retryAfter < wantSecs-5 || retryAfter > wantSecs+5 {
		t.Fatalf("Retry-After = %q, want ~%d", SafeResponseHeaders(err).Get("Retry-After"), wantSecs)
	}
}

// Every order of the accounts reports the earliest recovery, whichever account
// was attempted last, with cooling on or off.
func TestExhaustedClaudeReportsEarliestRecoveryAcrossAccounts(t *testing.T) {
	orders := [][]claudeAccountLimit{
		{fiveHourSoon, fiveHourLater, weeklyDrained},
		{weeklyDrained, fiveHourLater, fiveHourSoon},
		{fiveHourLater, weeklyDrained, fiveHourSoon},
	}
	for _, disableCooling := range []bool{true, false} {
		for i, order := range orders {
			for _, stream := range []bool{false, true} {
				name := "cooling-on"
				if disableCooling {
					name = "cooling-off"
				}
				name += "/order-" + strconv.Itoa(i)
				if stream {
					name += "/stream"
				}
				t.Run(name, func(t *testing.T) {
					manager, model := newClaudeAccountsManager(t, disableCooling, order...)
					checkClaudeReset(t, execClaudeAccounts(t, manager, model, stream), 15*time.Minute, "five_hour")
				})
			}
		}
	}
}

// An account whose weekly window is drained recovers at the weekly reset, not
// at the earlier session reset its top-level header names.
func TestExhaustedClaudeWeeklyDrainedReportsWeeklyReset(t *testing.T) {
	for _, disableCooling := range []bool{true, false} {
		t.Run("disable-cooling="+strconv.FormatBool(disableCooling), func(t *testing.T) {
			manager, model := newClaudeAccountsManager(t, disableCooling, weeklyDrained, fiveHourLater)
			checkClaudeReset(t, execClaudeAccounts(t, manager, model, false), 150*time.Minute, "five_hour")

			manager, model = newClaudeAccountsManager(t, disableCooling, weeklyDrained)
			checkClaudeReset(t, execClaudeAccounts(t, manager, model, false), 23*time.Hour, "seven_day")
		})
	}
}

// Without an exhausted window in an account's headers (an overage-only
// rejection, say) nothing says when it recovers, so the upstream headers stand.
func TestExhaustedClaudeWithoutWindowEvidenceKeepsUpstream(t *testing.T) {
	noWindow := claudeAccountLimit{
		fiveHour: [3]string{"allowed", "0.3", "2h"},
		sevenDay: [3]string{"allowed", "0.3", "72h"},
		reset:    3 * time.Hour,
		claim:    "five_hour",
	}
	manager, model := newClaudeAccountsManager(t, true, fiveHourLater, noWindow)
	err := execClaudeAccounts(t, manager, model, false)
	var adjusted *claudeCooldownAdjustedError
	if errors.As(err, &adjusted) {
		t.Fatalf("adjusted without evidence for every account: %v", err)
	}
	if got := ClaudeRateLimitResponseHeaders(err).Get("Anthropic-Ratelimit-Unified-Reset"); strings.TrimSpace(got) == "" {
		t.Fatal("upstream reset header missing")
	}
}
