package openai

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// responsesLongRetryThreshold separates a short cooldown, which Responses
// clients may wait out, from a long one. A model cooldown longer than this is
// reported as usage_limit_reached so Codex fails the turn at once with a reset
// time; any other Retry-After longer than this is capped to it so Codex does
// not sleep for hours between retries.
const responsesLongRetryThreshold = 60 * time.Second

const (
	codexPrimaryUsedPercentHeader = "x-codex-primary-used-percent"
	codexPrimaryResetAtHeader     = "x-codex-primary-reset-at"
	responsesUsageLimitErrorType  = "usage_limit_reached"
)

// responsesUsageLimitNow is the clock for reset computations; tests replace it.
var responsesUsageLimitNow = time.Now

// responsesErrorHeaders is the header set of a Responses error: real HTTP
// response headers, or the "headers" object of a websocket error event.
type responsesErrorHeaders interface {
	Get(key string) string
	Set(key, value string)
}

// WriteErrorResponse writes Responses errors with the Codex usage-limit and
// Retry-After rules applied. Other handlers keep the base behaviour.
func (h *OpenAIResponsesAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	h.BaseAPIHandler.WriteErrorResponseWith(c, msg, adjustResponsesHTTPErrorResponse)
}

func adjustResponsesHTTPErrorResponse(msg *interfaces.ErrorMessage, body []byte, header http.Header) []byte {
	var err error
	if msg != nil {
		err = msg.Error
	}
	return applyResponsesErrorPolicy(err, body, header)
}

// applyResponsesErrorPolicy rewrites a Responses error body ({"error":{...}})
// and its headers:
//   - a model cooldown longer than responsesLongRetryThreshold becomes
//     usage_limit_reached with resets_at plus x-codex-primary-* headers;
//   - an upstream usage_limit_reached body with a future resets_at gets the
//     x-codex-primary-* headers when they are missing;
//   - any other Retry-After longer than the threshold is capped to it.
func applyResponsesErrorPolicy(err error, body []byte, headers responsesErrorHeaders) []byte {
	if headers == nil {
		return body
	}
	now := responsesUsageLimitNow()
	var errorNode ojson.Result
	if len(body) > 0 && ojson.ValidBytes(body) {
		errorNode = ojson.GetBytesNoCopy(body, "error")
	}

	if errorNode.IsObject() && isResponsesModelCooldown(err) {
		if reset, ok := responsesCooldownReset(errorNode, headers, now); ok && reset > responsesLongRetryThreshold {
			resetsAt := ceilUnixSeconds(now.Add(reset))
			doc := ojson.NewDoc(body)
			if errSet := doc.Set("error.type", responsesUsageLimitErrorType); errSet != nil {
				return body
			}
			if errSet := doc.Set("error.resets_at", resetsAt); errSet != nil {
				return body
			}
			headers.Set(codexPrimaryUsedPercentHeader, "100")
			headers.Set(codexPrimaryResetAtHeader, strconv.FormatInt(resetsAt, 10))
			return doc.Bytes()
		}
	}

	if errorNode.IsObject() && strings.TrimSpace(errorNode.Get("type").String()) == responsesUsageLimitErrorType {
		// Codex classifies this body as a usage limit and does not retry, so
		// Retry-After does not matter here.
		resetsAt := errorNode.Get("resets_at").Int()
		if resetsAt > now.Unix() && strings.TrimSpace(headers.Get(codexPrimaryResetAtHeader)) == "" {
			if strings.TrimSpace(headers.Get(codexPrimaryUsedPercentHeader)) == "" {
				headers.Set(codexPrimaryUsedPercentHeader, "100")
			}
			headers.Set(codexPrimaryResetAtHeader, strconv.FormatInt(resetsAt, 10))
		}
		return body
	}

	if retryAfter, ok := parseResponsesRetryAfter(headers.Get("Retry-After"), now); ok && retryAfter > responsesLongRetryThreshold {
		headers.Set("Retry-After", strconv.FormatInt(int64(responsesLongRetryThreshold/time.Second), 10))
	}
	return body
}

func isResponsesModelCooldown(err error) bool {
	if err == nil {
		return false
	}
	var marker interface{ IsModelCooldown() bool }
	if errors.As(err, &marker) && marker != nil && marker.IsModelCooldown() {
		return true
	}
	var authErr *coreauth.Error
	return errors.As(err, &authErr) && authErr != nil && strings.EqualFold(strings.TrimSpace(authErr.Code), "model_cooldown")
}

// responsesCooldownReset returns how long the cooldown lasts: the Retry-After
// the client will see, or the body's reset_seconds when there is none.
func responsesCooldownReset(errorNode ojson.Result, headers responsesErrorHeaders, now time.Time) (time.Duration, bool) {
	if reset, ok := parseResponsesRetryAfter(headers.Get("Retry-After"), now); ok {
		return reset, true
	}
	if seconds := errorNode.Get("reset_seconds"); seconds.Exists() && seconds.Int() > 0 {
		return time.Duration(seconds.Int()) * time.Second, true
	}
	return 0, false
}

// parseResponsesRetryAfter parses a Retry-After value in seconds or HTTP-date form.
func parseResponsesRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(1<<63-1)/int64(time.Second) {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, errParse := http.ParseTime(value)
	if errParse != nil {
		return 0, false
	}
	if !at.After(now) {
		return 0, true
	}
	return at.Sub(now), true
}

func ceilUnixSeconds(t time.Time) int64 {
	seconds := t.Unix()
	if t.Nanosecond() > 0 {
		seconds++
	}
	return seconds
}

// responsesWebsocketHeaders is the ordered "headers" object of a websocket
// error event. Lookups ignore case; existing keys keep their spelling.
type responsesWebsocketHeaders struct {
	keys   []string
	values []string
}

func (h *responsesWebsocketHeaders) index(key string) int {
	for i, existing := range h.keys {
		if strings.EqualFold(existing, key) {
			return i
		}
	}
	return -1
}

func (h *responsesWebsocketHeaders) Get(key string) string {
	if i := h.index(key); i >= 0 {
		return h.values[i]
	}
	return ""
}

func (h *responsesWebsocketHeaders) Set(key, value string) {
	if i := h.index(key); i >= 0 {
		h.values[i] = value
		return
	}
	h.keys = append(h.keys, key)
	h.values = append(h.values, value)
}
