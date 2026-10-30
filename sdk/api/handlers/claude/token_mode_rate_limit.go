package claude

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Claude Code in token mode (ANTHROPIC_AUTH_TOKEN) ignores the unified
// rate-limit headers, so a 429 gives its host no reset time. Hosts such as T3
// Code resume a thread only when the final text reads
// "API Error: 503 [<provider>] [429]: {...rate_limit_error...} (reset after 1h 2m 3s)".
// For those clients a messages 429 with a known reset is rewritten as a 503 in
// that shape; x-should-retry: false stops the CLI from sleeping on retry-after.

const (
	claudeMessagesRequestKey = "cliproxy.claude.messages_request_model"

	claudeOAuthBeta               = "oauth-2025-04-20"
	claudeCLIUserAgentPrefix      = "claude-cli/"
	claudeDefaultRateLimitMessage = "This request would exceed your account's rate limit. Please try again later."
)

var claudeNow = time.Now

// markClaudeMessagesRequest flags c as a /v1/messages request so error writing
// may apply the token-mode rewrite. model is the requested model, if any.
func markClaudeMessagesRequest(c *gin.Context, model string) {
	if c != nil {
		c.Set(claudeMessagesRequestKey, model)
	}
}

// isClaudeCodeTokenMode reports whether the request comes from Claude Code
// authenticating with a bearer token rather than the OAuth subscription flow.
func isClaudeCodeTokenMode(h http.Header) bool {
	if !strings.HasPrefix(h.Get("User-Agent"), claudeCLIUserAgentPrefix) {
		return false
	}
	for _, line := range h.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(beta), claudeOAuthBeta) {
				return false
			}
		}
	}
	return true
}

// claudeResetSeconds returns the whole seconds until a rejected unified limit
// resets, or false when the headers do not describe a future reset.
func claudeResetSeconds(headers http.Header, now time.Time) (int64, bool) {
	if !strings.EqualFold(strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-Status")), "rejected") {
		return 0, false
	}
	reset, err := strconv.ParseInt(strings.TrimSpace(headers.Get("Anthropic-Ratelimit-Unified-Reset")), 10, 64)
	if err != nil {
		return 0, false
	}
	delta := time.Unix(reset, 0).Sub(now)
	secs := int64((delta + time.Second - 1) / time.Second)
	if delta <= 0 || secs <= 0 {
		return 0, false
	}
	return secs, true
}

// formatClaudeResetDuration renders seconds as "1h 2m 3s", omitting zero parts.
func formatClaudeResetDuration(seconds int64) string {
	h, m, s := seconds/3600, seconds%3600/60, seconds%60
	parts := make([]string, 0, 3)
	if h > 0 {
		parts = append(parts, strconv.FormatInt(h, 10)+"h")
	}
	if m > 0 {
		parts = append(parts, strconv.FormatInt(m, 10)+"m")
	}
	if s > 0 {
		parts = append(parts, strconv.FormatInt(s, 10)+"s")
	}
	return strings.Join(parts, " ")
}

func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func sanitizeClaudeProviderModel(model string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "]", "").Replace(model))
}

// buildClaudeTokenModeRateLimitBody builds the 503 body for a 429.
func buildClaudeTokenModeRateLimitBody(model, upstreamMessage string, seconds int64) ([]byte, error) {
	label := "[claude]"
	if m := sanitizeClaudeProviderModel(model); m != "" {
		label = "[claude/" + m + "]"
	}
	message := stripCRLF(strings.TrimSpace(upstreamMessage))
	if message == "" {
		message = claudeDefaultRateLimitMessage
	}
	quoted, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	inner := `{"type":"error","error":{"type":"rate_limit_error","message":` + string(quoted) + `}}`
	text := label + " [429]: " + inner + " (reset after " + formatClaudeResetDuration(seconds) + ")"
	return json.Marshal(struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Message string `json:"message"`
	}{Message: text}})
}
