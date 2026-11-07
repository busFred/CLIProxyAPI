package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

var usageLimitTestNow = time.Unix(1_800_000_000, 0)

func setUsageLimitTestClock(t *testing.T) {
	t.Helper()
	previous := responsesUsageLimitNow
	responsesUsageLimitNow = func() time.Time { return usageLimitTestNow }
	t.Cleanup(func() { responsesUsageLimitNow = previous })
}

// retryAfterTestError carries a trusted Retry-After the way auth_unavailable
// and Home cooldown errors do.
type retryAfterTestError struct {
	cause      *coreauth.Error
	retryAfter string
}

func (e *retryAfterTestError) Error() string   { return e.cause.Error() }
func (e *retryAfterTestError) Unwrap() error   { return e.cause }
func (e *retryAfterTestError) StatusCode() int { return e.cause.HTTPStatus }
func (e *retryAfterTestError) SafeResponseHeaders() http.Header {
	return http.Header{"Retry-After": {e.retryAfter}}
}

// codexWrappedErrorEvent mirrors the websocket error event fields Codex reads.
type codexWrappedErrorEvent struct {
	Type    string            `json:"type"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Error   *struct {
		Type     string `json:"type"`
		ResetsAt *int64 `json:"resets_at"`
	} `json:"error"`
}

// codexUsageLimitReset reports whether Codex would classify the event as a
// usage limit (429 with error.type usage_limit_reached) and the reset it shows.
func codexUsageLimitReset(t *testing.T, payload []byte) (int64, bool) {
	t.Helper()
	var event codexWrappedErrorEvent
	if errUnmarshal := json.Unmarshal(payload, &event); errUnmarshal != nil {
		t.Fatalf("payload does not decode as a Codex wrapped error event: %v: %s", errUnmarshal, payload)
	}
	if event.Type != "error" || event.Status != http.StatusTooManyRequests || event.Error == nil || event.Error.Type != "usage_limit_reached" || event.Error.ResetsAt == nil {
		return 0, false
	}
	return *event.Error.ResetsAt, true
}

type usageLimitCase struct {
	name string
	msg  *interfaces.ErrorMessage
	// want* describe the expected output; empty strings mean "absent".
	wantType        string
	wantResetsAt    int64
	wantRetryAfter  string
	wantUsedPercent string
	wantResetAt     string
}

func usageLimitCases() []usageLimitCase {
	nowUnix := usageLimitTestNow.Unix()
	upstreamUsageLimit := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":` + strconv.FormatInt(nowUnix+3600, 10) + `}}`
	return []usageLimitCase{
		{
			name:            "long cooldown",
			msg:             &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: coreauth.NewModelCooldownError("gpt-5.5", "codex", 2*time.Hour+300*time.Millisecond)},
			wantType:        "usage_limit_reached",
			wantResetsAt:    nowUnix + 7201,
			wantRetryAfter:  "7201",
			wantUsedPercent: "100",
			wantResetAt:     strconv.FormatInt(nowUnix+7201, 10),
		},
		{
			name: "home long cooldown",
			msg: &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: &retryAfterTestError{
				cause:      &coreauth.Error{Code: "model_cooldown", Message: "all credentials cooling", HTTPStatus: http.StatusTooManyRequests},
				retryAfter: "600",
			}},
			wantType:        "usage_limit_reached",
			wantResetsAt:    nowUnix + 600,
			wantRetryAfter:  "600",
			wantUsedPercent: "100",
			wantResetAt:     strconv.FormatInt(nowUnix+600, 10),
		},
		{
			name:           "short cooldown",
			msg:            &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: coreauth.NewModelCooldownError("gpt-5.5", "codex", 30*time.Second)},
			wantRetryAfter: "30",
		},
		{
			name:           "cooldown at threshold",
			msg:            &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: coreauth.NewModelCooldownError("gpt-5.5", "codex", 60*time.Second)},
			wantRetryAfter: "60",
		},
		{
			name:            "upstream usage limit without headers",
			msg:             &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New(upstreamUsageLimit)},
			wantType:        "usage_limit_reached",
			wantResetsAt:    nowUnix + 3600,
			wantUsedPercent: "100",
			wantResetAt:     strconv.FormatInt(nowUnix+3600, 10),
		},
		{
			name: "auth unavailable long retry",
			msg: &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: &retryAfterTestError{
				cause:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
				retryAfter: "3600",
			}},
			wantRetryAfter: "60",
		},
		{
			name: "http-date long retry",
			msg: &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: &retryAfterTestError{
				cause:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
				retryAfter: usageLimitTestNow.Add(2 * time.Hour).UTC().Format(http.TimeFormat),
			}},
			wantRetryAfter: "60",
		},
		{
			name: "short retry untouched",
			msg: &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: &retryAfterTestError{
				cause:      &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
				retryAfter: "45",
			}},
			wantRetryAfter: "45",
		},
	}
}

func checkUsageLimitOutput(t *testing.T, tc usageLimitCase, errorJSON []byte, header func(string) string) {
	t.Helper()
	if got := ojson.GetBytesNoCopy(errorJSON, "type").String(); tc.wantType != "" && got != tc.wantType {
		t.Fatalf("error.type = %q, want %q: %s", got, tc.wantType, errorJSON)
	} else if tc.wantType == "" && got == "usage_limit_reached" {
		t.Fatalf("error.type = usage_limit_reached, want unchanged: %s", errorJSON)
	}
	resetsAt := ojson.GetBytesNoCopy(errorJSON, "resets_at")
	if tc.wantResetsAt != 0 && resetsAt.Int() != tc.wantResetsAt {
		t.Fatalf("error.resets_at = %s, want %d", resetsAt.Raw, tc.wantResetsAt)
	}
	if tc.wantResetsAt == 0 && resetsAt.Exists() {
		t.Fatalf("error.resets_at = %s, want absent", resetsAt.Raw)
	}
	for name, want := range map[string]string{
		"Retry-After":                 tc.wantRetryAfter,
		codexPrimaryUsedPercentHeader: tc.wantUsedPercent,
		codexPrimaryResetAtHeader:     tc.wantResetAt,
	} {
		if got := header(name); got != want {
			t.Fatalf("header %s = %q, want %q", name, got, want)
		}
	}
}

func websocketHeaderLookup(payload []byte) func(string) string {
	return func(name string) string {
		var value string
		ojson.GetBytesNoCopy(payload, "headers").ForEach(func(key, item ojson.Result) bool {
			if strings.EqualFold(key.String(), name) {
				value = item.String()
				return false
			}
			return true
		})
		return value
	}
}

func TestResponsesWebsocketErrorPayloadUsageLimitPolicy(t *testing.T) {
	setUsageLimitTestClock(t)
	for _, tc := range usageLimitCases() {
		t.Run(tc.name, func(t *testing.T) {
			payload, errBuild := buildResponsesWebsocketErrorPayload(tc.msg)
			if errBuild != nil {
				t.Fatalf("build payload: %v", errBuild)
			}
			if got := ojson.GetBytesNoCopy(payload, "status").Int(); int(got) != tc.msg.StatusCode {
				t.Fatalf("status = %d, want %d: %s", got, tc.msg.StatusCode, payload)
			}
			checkUsageLimitOutput(t, tc, []byte(ojson.GetBytesNoCopy(payload, "error").Raw), websocketHeaderLookup(payload))

			resetsAt, isUsageLimit := codexUsageLimitReset(t, payload)
			if want := tc.wantType == "usage_limit_reached"; isUsageLimit != want {
				t.Fatalf("Codex usage-limit classification = %t, want %t: %s", isUsageLimit, want, payload)
			}
			if isUsageLimit && resetsAt != tc.wantResetsAt {
				t.Fatalf("Codex resets_at = %d, want %d", resetsAt, tc.wantResetsAt)
			}
		})
	}
}

func TestResponsesWebsocketErrorPayloadKeepsModelCooldownFields(t *testing.T) {
	setUsageLimitTestClock(t)
	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("gpt-5.5", "codex", time.Hour),
	})
	if errBuild != nil {
		t.Fatalf("build payload: %v", errBuild)
	}
	for path, want := range map[string]string{
		"error.code":          "model_cooldown",
		"error.model":         "gpt-5.5",
		"error.provider":      "codex",
		"error.reset_seconds": "3600",
	} {
		if got := ojson.GetBytesNoCopy(payload, path).String(); got != want {
			t.Fatalf("%s = %q, want %q: %s", path, got, want, payload)
		}
	}
}

func TestResponsesWebsocketErrorPayloadKeepsUpstreamCodexHeaders(t *testing.T) {
	setUsageLimitTestClock(t)
	body := `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":` + strconv.FormatInt(usageLimitTestNow.Unix()+3600, 10) + `}}`
	payload, errBuild := buildResponsesWebsocketErrorPayload(&interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(body),
		Addon: http.Header{
			"X-Codex-Primary-Used-Percent": {"97"},
			"X-Codex-Primary-Reset-At":     {"1800000123"},
			"Retry-After":                  {"3600"},
		},
	})
	if errBuild != nil {
		t.Fatalf("build payload: %v", errBuild)
	}
	lookup := websocketHeaderLookup(payload)
	if got := lookup(codexPrimaryUsedPercentHeader); got != "97" {
		t.Fatalf("used percent = %q, want 97: %s", got, payload)
	}
	if got := lookup(codexPrimaryResetAtHeader); got != "1800000123" {
		t.Fatalf("reset at = %q, want 1800000123: %s", got, payload)
	}
	if got := lookup("Retry-After"); got != "3600" {
		t.Fatalf("Retry-After = %q, want untouched 3600 on a usage-limit body: %s", got, payload)
	}
	if count := strings.Count(strings.ToLower(ojson.GetBytesNoCopy(payload, "headers").Raw), codexPrimaryResetAtHeader); count != 1 {
		t.Fatalf("reset-at header appears %d times, want 1: %s", count, payload)
	}
}

func TestResponsesHTTPErrorResponseUsageLimitPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setUsageLimitTestClock(t)
	for _, tc := range usageLimitCases() {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			NewOpenAIResponsesAPIHandler(&handlers.BaseAPIHandler{}).WriteErrorResponse(c, tc.msg)

			if recorder.Code != tc.msg.StatusCode {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.msg.StatusCode)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			checkUsageLimitOutput(t, tc, []byte(ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error").Raw), recorder.Header().Get)
		})
	}
}

func TestResponsesHTTPDirectErrorResponseUsageLimitHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setUsageLimitTestClock(t)
	resetsAt := strconv.FormatInt(usageLimitTestNow.Unix()+3600, 10)
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"limit","resets_at":` + resetsAt + `}}`)

	for _, tc := range []struct {
		name            string
		headers         http.Header
		wantUsedPercent string
		wantResetAt     string
	}{
		{name: "headers missing", headers: http.Header{"Content-Type": {"application/json"}}, wantUsedPercent: "100", wantResetAt: resetsAt},
		{name: "headers present", headers: http.Header{"X-Codex-Primary-Used-Percent": {"97"}, "X-Codex-Primary-Reset-At": {"1800000123"}}, wantUsedPercent: "97", wantResetAt: "1800000123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			NewOpenAIResponsesAPIHandler(&handlers.BaseAPIHandler{}).WriteErrorResponse(c, &interfaces.ErrorMessage{
				StatusCode:     http.StatusTooManyRequests,
				DirectResponse: true,
				Body:           body,
				Headers:        tc.headers,
			})
			if got := recorder.Body.String(); got != string(body) {
				t.Fatalf("body = %s, want unchanged %s", got, body)
			}
			if got := recorder.Header().Get(codexPrimaryUsedPercentHeader); got != tc.wantUsedPercent {
				t.Fatalf("used percent = %q, want %q", got, tc.wantUsedPercent)
			}
			if got := recorder.Header().Get(codexPrimaryResetAtHeader); got != tc.wantResetAt {
				t.Fatalf("reset at = %q, want %q", got, tc.wantResetAt)
			}
		})
	}
}

func TestChatCompletionsErrorResponseKeepsLongCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setUsageLimitTestClock(t)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	NewOpenAIAPIHandler(&handlers.BaseAPIHandler{}).WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("gpt-5.5", "codex", 2*time.Hour),
	})
	if got := recorder.Header().Get("Retry-After"); got != "7200" {
		t.Fatalf("Retry-After = %q, want 7200", got)
	}
	if got := recorder.Header().Get(codexPrimaryResetAtHeader); got != "" {
		t.Fatalf("reset-at header = %q, want absent", got)
	}
	if got := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.type"); got.Exists() {
		t.Fatalf("error.type = %s, want absent: %s", got.Raw, recorder.Body.String())
	}
	if got := ojson.GetBytesNoCopy(recorder.Body.Bytes(), "error.code").String(); got != "model_cooldown" {
		t.Fatalf("error.code = %q, want model_cooldown", got)
	}
}

// newCoolingResponsesRouter registers one Codex credential that is cooling
// down for 8250 s, so the auth manager returns a real model cooldown.
func newCoolingResponsesRouter(t *testing.T, model string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	executor := &websocketProviderCaptureExecutor{provider: "codex"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	authID := model + "-auth"
	next := time.Now().Add(8250 * time.Second)
	auth := &coreauth.Auth{
		ID:             authID,
		Provider:       executor.Identifier(),
		Status:         coreauth.StatusActive,
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: next},
		LastError:      &coreauth.Error{Code: "usage_limit_reached", Message: "upstream usage limit", HTTPStatus: http.StatusTooManyRequests},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	router.GET("/v1/responses", h.ResponsesWebsocket)
	return router
}

func assertCoolingResetsAt(t *testing.T, resetsAt int64, resetAtHeader string, before time.Time) {
	t.Helper()
	low := before.Add(8200 * time.Second).Unix()
	high := time.Now().Add(8260 * time.Second).Unix()
	if resetsAt < low || resetsAt > high {
		t.Fatalf("resets_at = %d, want within [%d, %d]", resetsAt, low, high)
	}
	if resetAtHeader != strconv.FormatInt(resetsAt, 10) {
		t.Fatalf("x-codex-primary-reset-at = %q, want %d", resetAtHeader, resetsAt)
	}
}

func TestResponsesRouteReportsRealCooldownAsUsageLimit(t *testing.T) {
	const model = "codex-usage-limit-http-model"
	router := newCoolingResponsesRouter(t, model)
	for _, streaming := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(streaming), func(t *testing.T) {
			before := time.Now()
			requestBody := `{"model":"` + model + `","input":"hi","stream":` + strconv.FormatBool(streaming) + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429; body=%s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json (not an SSE stream)", got)
			}
			body := recorder.Body.Bytes()
			if got := ojson.GetBytesNoCopy(body, "error.type").String(); got != "usage_limit_reached" {
				t.Fatalf("error.type = %q, want usage_limit_reached: %s", got, body)
			}
			if got := ojson.GetBytesNoCopy(body, "error.code").String(); got != "model_cooldown" {
				t.Fatalf("error.code = %q, want model_cooldown kept: %s", got, body)
			}
			retryAfter, errRetry := strconv.ParseInt(recorder.Header().Get("Retry-After"), 10, 64)
			if errRetry != nil || retryAfter < 8200 {
				t.Fatalf("Retry-After = %q, want the full cooldown", recorder.Header().Get("Retry-After"))
			}
			if got := recorder.Header().Get(codexPrimaryUsedPercentHeader); got != "100" {
				t.Fatalf("used percent = %q, want 100", got)
			}
			assertCoolingResetsAt(t, ojson.GetBytesNoCopy(body, "error.resets_at").Int(), recorder.Header().Get(codexPrimaryResetAtHeader), before)
		})
	}
}

func TestResponsesWebsocketReportsRealCooldownAsUsageLimit(t *testing.T) {
	const model = "codex-usage-limit-ws-model"
	server := httptest.NewServer(newCoolingResponsesRouter(t, model))
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	before := time.Now()
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"`+model+`","input":[]}`)); errWrite != nil {
		t.Fatalf("write request: %v", errWrite)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read error event: %v", errRead)
	}
	resetsAt, isUsageLimit := codexUsageLimitReset(t, payload)
	if !isUsageLimit {
		t.Fatalf("Codex would not classify the event as a usage limit: %s", payload)
	}
	lookup := websocketHeaderLookup(payload)
	if got := lookup(codexPrimaryUsedPercentHeader); got != "100" {
		t.Fatalf("used percent = %q, want 100: %s", got, payload)
	}
	if retryAfter, errRetry := strconv.ParseInt(lookup("Retry-After"), 10, 64); errRetry != nil || retryAfter < 8200 {
		t.Fatalf("Retry-After = %q, want the full cooldown: %s", lookup("Retry-After"), payload)
	}
	assertCoolingResetsAt(t, resetsAt, lookup(codexPrimaryResetAtHeader), before)
}
