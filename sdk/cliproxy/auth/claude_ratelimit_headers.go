package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	return e
}

func (e *modelCooldownError) claudeRateLimitHeaders() http.Header {
	if e == nil || e.claudeHint == nil || e.claudeHint.claim == "" || e.claudeHint.resetAt.IsZero() {
		return nil
	}
	resetUnix := e.claudeHint.resetAt.Unix()
	if e.claudeHint.resetAt.Nanosecond() > 0 {
		resetUnix++
	}
	headers := make(http.Header)
	headers.Set(claudeUnifiedStatusHeader, claudeUnifiedStatusReject)
	headers.Set(claudeUnifiedResetHeader, strconv.FormatInt(resetUnix, 10))
	headers.Set(claudeUnifiedClaimHeader, e.claudeHint.claim)
	return headers
}

// claudeRateLimitHintFor builds the hint for a cooldown that blocks every
// candidate. It returns nil unless all candidates are Claude credentials and the
// cooldown is credibly a usage window rather than short backoff.
//
// The reset instant is the earliest recovery among candidates (the same value
// resetIn is derived from), which is when a retry can first succeed. The claim
// comes from the upstream headers recorded on the credential that recovers
// first; without that evidence a cooldown shorter than five minutes is left to
// plain Retry-After, because it is indistinguishable from request-rate backoff
// and must not be presented as a session or weekly limit.
func claudeRateLimitHintFor(candidates []cooldownCandidate, model string, resetIn time.Duration, now time.Time) *claudeRateLimitHint {
	if len(candidates) == 0 {
		return nil
	}
	var first *cooldownCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.auth == nil || !strings.EqualFold(strings.TrimSpace(candidate.auth.Provider), "claude") {
			return nil
		}
		if candidate.next.IsZero() {
			continue
		}
		if first == nil || candidate.next.Before(first.next) {
			first = candidate
		}
	}
	if first == nil {
		return nil
	}
	resetAt := first.next
	if !resetAt.After(now) {
		return nil
	}
	if resetIn <= 0 {
		resetIn = resetAt.Sub(now)
	}

	claim := claudeClaimFromSignals(claudeSignalsForModel(first.auth, model))
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

// claudeSignalsForModel returns the recorded upstream headers for the model
// state when present and the credential-level snapshot otherwise.
func claudeSignalsForModel(auth *Auth, model string) map[string]string {
	if auth == nil {
		return nil
	}
	if model != "" {
		modelKey := canonicalModelKey(model)
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if len(state.Quota.Signals) > 0 {
				return state.Quota.Signals
			}
		}
	}
	return auth.Quota.Signals
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
