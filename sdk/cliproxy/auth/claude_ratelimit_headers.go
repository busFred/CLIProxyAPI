package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Header names and values Claude Code reads from a 429 to render
// "limit reached - resets <time>". The reset is a Unix timestamp in seconds and
// the representative claim names the exhausted window.
const (
	claudeUnifiedRateLimitHeaderPrefix = "anthropic-ratelimit-unified-"

	claudeUnifiedStatusHeader  = "Anthropic-Ratelimit-Unified-Status"
	claudeUnifiedResetHeader   = "Anthropic-Ratelimit-Unified-Reset"
	claudeUnifiedClaimHeader   = "Anthropic-Ratelimit-Unified-Representative-Claim"
	claudeUnified5hStatusKey   = "Anthropic-Ratelimit-Unified-5h-Status"
	claudeUnified7dStatusKey   = "Anthropic-Ratelimit-Unified-7d-Status"
	claudeUnifiedStatusReject  = "rejected"
	claudeClaimFiveHour        = "five_hour"
	claudeClaimSevenDay        = "seven_day"
	claudeClaimSevenDayOpus    = "seven_day_opus"
	claudeClaimSevenDaySonnet  = "seven_day_sonnet"
	claudeFiveHourWindow       = 5 * time.Hour
	claudeMinSyntheticCooldown = 5 * time.Minute
)

// FilterClaudeUnifiedRateLimitHeaders returns a copy of src that holds only the
// anthropic-ratelimit-unified-* headers (matched case-insensitively). Nothing
// else is forwarded: the allowlist is the prefix and no other header passes.
// It returns nil when src carries no such header.
func FilterClaudeUnifiedRateLimitHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	var dst http.Header
	for key, values := range src {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), claudeUnifiedRateLimitHeaderPrefix) {
			continue
		}
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(key))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if !validQuotaSignalValue(value) {
				continue
			}
			if dst == nil {
				dst = make(http.Header)
			}
			dst.Add(canonical, value)
		}
	}
	return dst
}

// ClaudeRateLimitResponseHeaders returns the anthropic-ratelimit-unified-*
// headers a Claude-format endpoint should send with err, or nil.
//
// Real upstream headers captured from an Anthropic 429 win over the synthetic
// set a proxy-generated model cooldown computes. The result is limited to the
// unified prefix and is intentionally separate from SafeResponseHeaders so
// non-Claude endpoints keep their existing response headers.
func ClaudeRateLimitResponseHeaders(err error) http.Header {
	if err == nil {
		return nil
	}
	var upstream interface{ ClaudeRateLimitHeaders() http.Header }
	if errors.As(err, &upstream) && upstream != nil {
		if headers := FilterClaudeUnifiedRateLimitHeaders(upstream.ClaudeRateLimitHeaders()); len(headers) > 0 {
			return headers
		}
	}
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) && cooldown != nil {
		return cooldown.claudeRateLimitHeaders()
	}
	return nil
}

// cooldownCandidate is one credential that blocks a model, with the instant it
// recovers.
type cooldownCandidate struct {
	auth *Auth
	next time.Time
}

// claudeRateLimitHint is what a model cooldown knows about the Anthropic window
// that exhausted every candidate.
type claudeRateLimitHint struct {
	claim   string
	resetAt time.Time
}

// withClaudeRateLimitHint records Claude usage-limit details on the error when
// every candidate is a Claude credential. It is a no-op otherwise.
func (e *modelCooldownError) withClaudeRateLimitHint(candidates []cooldownCandidate, model string, now time.Time) *modelCooldownError {
	if e == nil {
		return e
	}
	e.claudeHint = claudeRateLimitHintFor(candidates, model, e.resetIn, now)
	// The reported unified reset is when an account is truly available, which
	// can be later than the earliest proxy cooldown expiry (an exhausted window
	// outlasting a short cooldown). Retry-After and the message must agree
	// with it, so resetIn follows the hint whenever the hint is later.
	if e.claudeHint != nil {
		if untilHint := e.claudeHint.resetAt.Sub(now); untilHint > e.resetIn {
			e.resetIn = untilHint
		}
	}
	return e
}

func (e *modelCooldownError) claudeRateLimitHeaders() http.Header {
	if e == nil || e.claudeHint == nil || e.claudeHint.claim == "" || e.claudeHint.resetAt.IsZero() {
		return nil
	}
	resetUnix := claudeResetUnix(e.claudeHint.resetAt)
	headers := make(http.Header)
	headers.Set(claudeUnifiedStatusHeader, claudeUnifiedStatusReject)
	headers.Set(claudeUnifiedResetHeader, strconv.FormatInt(resetUnix, 10))
	headers.Set(claudeUnifiedClaimHeader, e.claudeHint.claim)
	return headers
}

// claudeResetUnix is the whole-second Unix reset clients see: the instant
// rounded up, so a client never resumes before the credential recovers.
func claudeResetUnix(t time.Time) int64 {
	unix := t.Unix()
	if t.Nanosecond() > 0 {
		unix++
	}
	return unix
}

// claudeCooldownAdjustedError wraps the upstream 429 returned after the
// conductor exhausted every Claude credential. The upstream error reports only
// the last attempted account's reset, which can be later than the earliest
// recovery across all accounts, so clients would wait longer than needed. The
// wrapper keeps the upstream error reachable through Unwrap (status, body,
// identity, usage-record status and cooldown side effects are untouched) and
// overrides only the unified headers and Retry-After.
type claudeCooldownAdjustedError struct {
	cause   error
	headers http.Header
	resetAt time.Time
}

func (e *claudeCooldownAdjustedError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *claudeCooldownAdjustedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// ClaudeRateLimitHeaders returns the upstream unified headers with the reset
// (and representative claim) of the earliest recovery.
func (e *claudeCooldownAdjustedError) ClaudeRateLimitHeaders() http.Header {
	if e == nil {
		return nil
	}
	return e.headers.Clone()
}

// Headers forwards any upstream response headers the wrapped error carries so
// header passthrough keeps working.
func (e *claudeCooldownAdjustedError) Headers() http.Header {
	if e == nil || e.cause == nil {
		return nil
	}
	var carrier interface{ Headers() http.Header }
	if errors.As(e.cause, &carrier) && carrier != nil {
		return carrier.Headers()
	}
	return nil
}

// SafeResponseHeaders reports Retry-After as the time to the earliest
// recovery, consistent with the adjusted unified reset.
func (e *claudeCooldownAdjustedError) SafeResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	return safeRetryAfterHeader(time.Until(e.resetAt))
}

// adjustClaudeUnifiedHeaders replaces the upstream unified reset and claim with
// the cooldown hint's. The hint is the earliest moment any credential is truly
// available, and the upstream account is one of its candidates, so it already
// covers that account's other exhausted windows; the upstream header alone can
// be earlier than that (5h resets soon while 7d is exhausted). The status stays
// as is and every other header is preserved. ok is false when nothing needs to
// change: no rejected status or reset upstream, no hint, or identical values.
func adjustClaudeUnifiedHeaders(upstream http.Header, hint *claudeRateLimitHint) (http.Header, bool) {
	if len(upstream) == 0 || hint == nil || hint.claim == "" || hint.resetAt.IsZero() {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(upstream.Get(claudeUnifiedStatusHeader)), claudeUnifiedStatusReject) {
		return nil, false
	}
	upstreamReset, err := strconv.ParseInt(strings.TrimSpace(upstream.Get(claudeUnifiedResetHeader)), 10, 64)
	if err != nil {
		return nil, false
	}
	cooldownReset := claudeResetUnix(hint.resetAt)
	if cooldownReset == upstreamReset &&
		strings.EqualFold(strings.TrimSpace(upstream.Get(claudeUnifiedClaimHeader)), hint.claim) {
		return nil, false
	}
	adjusted := upstream.Clone()
	adjusted.Set(claudeUnifiedResetHeader, strconv.FormatInt(cooldownReset, 10))
	adjusted.Set(claudeUnifiedClaimHeader, hint.claim)
	return adjusted, true
}

// withEarliestClaudeCooldownReset adjusts the final error of a request that
// exhausted every Claude credential. When err is an upstream 429 with rejected
// unified headers and every credential is known to be unavailable (a model
// cooldown, or recorded windows that have not reset), the reported reset and
// claim become those of the earliest recovery. Otherwise err is returned
// unchanged. Home mode owns its own credential pool and timing, so it is left
// alone.
func (m *Manager) withEarliestClaudeCooldownReset(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, err error) error {
	if m == nil || err == nil || m.HomeEnabled() || statusCodeFromError(err) != http.StatusTooManyRequests {
		return err
	}
	var carrier interface{ ClaudeRateLimitHeaders() http.Header }
	if !errors.As(err, &carrier) || carrier == nil {
		return err
	}
	upstream := FilterClaudeUnifiedRateLimitHeaders(carrier.ClaudeRateLimitHeaders())
	if len(upstream) == 0 {
		return err
	}
	now := time.Now()
	hint := m.claudeCooldownHintForModel(ctx, providers, model, opts, now)
	adjusted, ok := adjustClaudeUnifiedHeaders(upstream, hint)
	if !ok {
		return err
	}
	return &claudeCooldownAdjustedError{cause: err, headers: adjusted, resetAt: hint.resetAt}
}

// claudeCooldownHintForModel returns the hint a fresh request for model would
// carry on its model cooldown error. Without that error (cooling disabled, or a
// cooldown that expired before its window reset) it falls back to the recorded
// unified windows. It returns nil when some eligible credential may be
// available, for mixed providers or for short backoff. It ignores the
// per-request tried set and has no selection side effects.
func (m *Manager) claudeCooldownHintForModel(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, now time.Time) *claudeRateLimitHint {
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if p := canonicalSchedulingProvider(provider); p != "" {
			providerSet[p] = struct{}{}
		}
	}
	if len(providerSet) == 0 {
		return nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	registryRef := registry.GetGlobalRegistry()
	modelKey := strings.TrimSpace(model)

	m.mu.RLock()
	defer m.mu.RUnlock()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(candidate) {
			continue
		}
		providerKey := executorKeyFromAuth(candidate)
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, ok := m.executorLocked(providerKey); !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil
	}
	_, err := m.availableAuthsForRouteModelAcrossPriorities(candidates, "mixed", model, now)
	var cooldown *modelCooldownError
	if errors.As(err, &cooldown) && cooldown != nil {
		return cooldown.claudeHint
	}
	if err != nil {
		return nil
	}
	return m.claudeSignalHintLocked(candidates, model, now)
}

// claudeSignalHintLocked builds the hint from the recorded unified windows when
// the proxy holds no cooldown that blocks every candidate: cooling is disabled,
// or a cooldown expired before its window reset. A credential not in cooldown
// counts as available now unless its signals show an exhausted window that has
// not reset, so one credential without that evidence yields nil and the
// upstream headers stand. Callers hold m.mu.
func (m *Manager) claudeSignalHintLocked(auths []*Auth, model string, now time.Time) *claudeRateLimitHint {
	candidates := make([]cooldownCandidate, 0, len(auths))
	for _, candidate := range auths {
		blocked, reason, next := isAuthBlockedForModel(candidate, m.selectionModelForAuth(candidate, model), now)
		switch {
		case !blocked:
			next = now
		case reason != blockReasonCooldown:
			continue
		}
		candidates = append(candidates, cooldownCandidate{auth: candidate, next: next})
	}
	return claudeRateLimitHintFor(candidates, model, 0, now)
}

// claudeRateLimitHintFor builds the hint for a cooldown that blocks every
// candidate. It returns nil unless all candidates are Claude credentials and the
// cooldown is credibly a usage window rather than short backoff.
//
// The reset instant is the earliest moment any candidate becomes available for
// the model (see claudeAvailableAt), which is when a retry can first succeed.
// The claim is the window that held the winning credential back; without that
// evidence it comes from the recorded upstream headers, and a cooldown shorter
// than five minutes is left to plain Retry-After, because it is
// indistinguishable from request-rate backoff and must not be presented as a
// session or weekly limit.
func claudeRateLimitHintFor(candidates []cooldownCandidate, model string, resetIn time.Duration, now time.Time) *claudeRateLimitHint {
	if len(candidates) == 0 {
		return nil
	}
	var (
		first      *cooldownCandidate
		firstAt    time.Time
		firstClaim string
		firstSigs  map[string]string
	)
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.auth == nil || !strings.EqualFold(strings.TrimSpace(candidate.auth.Provider), "claude") {
			return nil
		}
		if candidate.next.IsZero() {
			continue
		}
		signals, modelScoped := claudeSignalsForModel(candidate.auth, model)
		at, claim := claudeAvailableAt(signals, modelScoped, candidate.next, now)
		if first == nil || at.Before(firstAt) {
			first, firstAt, firstClaim, firstSigs = candidate, at, claim, signals
		}
	}
	if first == nil {
		return nil
	}
	resetAt := firstAt
	if !resetAt.After(now) {
		return nil
	}
	if resetIn <= 0 || resetAt.After(first.next) {
		resetIn = resetAt.Sub(now)
	}

	claim := firstClaim
	if claim == "" {
		claim = claudeClaimFromSignals(firstSigs)
	}
	if claim == "" {
		if resetIn < claudeMinSyntheticCooldown {
			return nil
		}
		// A 5 hour window cannot reset later than five hours from now, so a
		// longer cooldown can only be the 7 day window. A shorter one is
		// reported as the session window; either way the reset timestamp, which
		// is what clients act on, is exact.
		claim = claudeClaimFiveHour
		if resetIn > claudeFiveHourWindow {
			claim = claudeClaimSevenDay
		}
	}
	return &claudeRateLimitHint{claim: claim, resetAt: resetAt}
}

// claudeAvailableAt returns when a credential can serve the model again: the
// later of the proxy's own cooldown expiry and the latest reset among the
// unified windows Anthropic reported as exhausted for it. A window is
// exhausted when its status is rejected or its utilization is at least 1; a
// window that is not exhausted never delays the credential, however soon it
// resets. claim names the window that set the returned time and is empty when
// the proxy cooldown (or a lack of window signals) decided it.
//
// The shared 5h and 7d windows always apply. A model-scoped weekly window
// (7d_<code>, e.g. the Fable weekly limit) applies only when the signals were
// recorded for the requested model itself; the credential-level snapshot may
// hold one learned from another model's response, which must not block this
// model, so it is ignored there. Resets that already passed are ignored.
func claudeAvailableAt(signals map[string]string, modelScoped bool, next, now time.Time) (time.Time, string) {
	at, claim := next, ""
	for _, window := range claudeUnifiedWindows(signals, modelScoped) {
		if !window.exhausted || !window.reset.After(now) || !window.reset.After(at) {
			continue
		}
		at, claim = window.reset, window.claim
	}
	return at, claim
}

type claudeUnifiedWindow struct {
	claim     string
	reset     time.Time
	exhausted bool
}

func claudeUnifiedWindows(signals map[string]string, modelScoped bool) []claudeUnifiedWindow {
	if len(signals) == 0 {
		return nil
	}
	names := map[string]string{}
	for key := range signals {
		lower := strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(lower, claudeUnifiedRateLimitHeaderPrefix) {
			continue
		}
		rest := strings.TrimPrefix(lower, claudeUnifiedRateLimitHeaderPrefix)
		for _, suffix := range []string{"-status", "-utilization", "-reset"} {
			if strings.HasSuffix(rest, suffix) {
				window := strings.TrimSuffix(rest, suffix)
				claim := ""
				switch {
				case window == "5h":
					claim = claudeClaimFiveHour
				case window == "7d":
					claim = claudeClaimSevenDay
				case strings.HasPrefix(window, "7d_") && modelScoped:
					claim = claudeClaimSevenDay
				}
				if claim != "" {
					names[window] = claim
				}
			}
		}
	}
	lookup := func(window, field string) string {
		want := claudeUnifiedRateLimitHeaderPrefix + window + field
		for key, value := range signals {
			if strings.EqualFold(strings.TrimSpace(key), want) {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	out := make([]claudeUnifiedWindow, 0, len(names))
	for window, claim := range names {
		exhausted := strings.EqualFold(lookup(window, "-status"), claudeUnifiedStatusReject)
		if !exhausted {
			if util, err := strconv.ParseFloat(lookup(window, "-utilization"), 64); err == nil && util >= 1 {
				exhausted = true
			}
		}
		out = append(out, claudeUnifiedWindow{claim: claim, reset: parseClaudeSignalReset(lookup(window, "-reset")), exhausted: exhausted})
	}
	return out
}

// parseClaudeSignalReset reads a window reset sent as Unix seconds or RFC 3339.
func parseClaudeSignalReset(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

// claudeSignalsForModel returns the recorded upstream headers for the model
// state when present, and the credential-level snapshot otherwise. modelScoped
// reports that the signals were recorded for the model itself, so model-scoped
// weekly windows in them belong to it; the credential-level snapshot mixes
// models and offers no such guarantee.
func claudeSignalsForModel(auth *Auth, model string) (signals map[string]string, modelScoped bool) {
	if auth == nil {
		return nil, false
	}
	if model != "" {
		modelKey := canonicalModelKey(model)
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if len(state.Quota.Signals) > 0 {
				return state.Quota.Signals, true
			}
		}
	}
	return auth.Quota.Signals, false
}

// claudeClaimFromSignals derives the exhausted window from recorded Anthropic
// headers. A rejected 7d window wins over a rejected 5h window because it
// recovers later; otherwise the upstream representative claim is used when it
// names a plain subscription window. Overage claims are not a usage window and
// yield "".
func claudeClaimFromSignals(signals map[string]string) string {
	if len(signals) == 0 {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(signals[claudeUnified7dStatusKey]), claudeUnifiedStatusReject) {
		return claudeClaimSevenDay
	}
	// Without a rejected status the claim may describe an allowed or warning
	// response rather than the window that put the credential in cooldown.
	if strings.EqualFold(strings.TrimSpace(signals[claudeUnifiedStatusHeader]), claudeUnifiedStatusReject) {
		claim := strings.ToLower(strings.TrimSpace(signals[claudeUnifiedClaimHeader]))
		switch claim {
		case claudeClaimFiveHour, claudeClaimSevenDay, claudeClaimSevenDayOpus, claudeClaimSevenDaySonnet:
			return claim
		}
	}
	if strings.EqualFold(strings.TrimSpace(signals[claudeUnified5hStatusKey]), claudeUnifiedStatusReject) {
		return claudeClaimFiveHour
	}
	return ""
}
