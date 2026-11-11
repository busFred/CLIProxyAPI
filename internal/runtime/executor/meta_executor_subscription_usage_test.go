package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The first event is stale; the one after response.completed carries the
// latest usage and must win.
const metaSubscriptionUsageTestSSE = "event: response.subscription_usage\n" +
	"data: {\"type\":\"response.subscription_usage\",\"subscription\":{\"tier\":\"1234567890123456\",\"window\":{\"used_percent\":1,\"resets_at\":1794425000,\"window_duration_mins\":300},\"weekly\":{\"used_percent\":60,\"resets_at\":1794787200}}}\n\n" +
	"event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"muse-spark-1.3\"}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"muse-spark-1.3\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n" +
	"event: response.subscription_usage\n" +
	"data: {\"type\":\"response.subscription_usage\",\"subscription\":{\"tier\":\"1234567890123456\",\"window\":{\"used_percent\":2,\"resets_at\":1794425416,\"window_duration_mins\":300},\"weekly\":{\"used_percent\":61,\"resets_at\":1794787200}}}\n\n"

func TestMetaExecutor_SubscriptionUsageReachesUsageRecordHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, metaSubscriptionUsageTestSSE)
	}))
	defer server.Close()

	exec := NewMetaExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "meta",
		Attributes: map[string]string{"api_key": "meta-token", "base_url": server.URL},
	}
	payload := []byte(`{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hello"}]}`)
	want := map[string]string{
		"X-Muse-Subscription-Window-Used-Percent": "2",
		"X-Muse-Subscription-Window-Resets-At":    "1794425416",
		"X-Muse-Subscription-Window-Minutes":      "300",
		"X-Muse-Subscription-Weekly-Used-Percent": "61",
		"X-Muse-Subscription-Weekly-Resets-At":    "1794787200",
	}

	run := func(t *testing.T, stream bool) {
		alias := "meta-subscription-usage-" + t.Name()
		capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
		coreusage.RegisterNamedPlugin(t.Name(), capture)
		t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{}) })

		ctx := logging.WithResponseHeadersHolder(coreusage.WithRequestedModelAlias(context.Background(), alias))
		req := cliproxyexecutor.Request{Model: "muse-spark-1.3", Payload: payload}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Stream: stream}

		var clientHeaders http.Header
		var body strings.Builder
		if stream {
			result, errStream := exec.ExecuteStream(ctx, auth, req, opts)
			if errStream != nil {
				t.Fatalf("ExecuteStream() error = %v", errStream)
			}
			clientHeaders = result.Headers
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("chunk error = %v", chunk.Err)
				}
				body.Write(chunk.Payload)
			}
		} else {
			resp, errExec := exec.Execute(ctx, auth, req, opts)
			if errExec != nil {
				t.Fatalf("Execute() error = %v", errExec)
			}
			clientHeaders = resp.Headers
			body.Write(resp.Payload)
		}

		if strings.Contains(body.String(), "subscription") {
			t.Fatalf("subscription telemetry leaked into client body: %s", body.String())
		}
		for key := range want {
			if clientHeaders.Get(key) != "" {
				t.Fatalf("client response headers carry %s", key)
			}
		}
		record := capture.await(t)
		for key, value := range want {
			if got := record.ResponseHeaders.Get(key); got != value {
				t.Errorf("record header %s = %q, want %q (all: %#v)", key, got, value, record.ResponseHeaders)
			}
		}
		if record.ResponseHeaders.Get("Content-Type") != "text/event-stream" {
			t.Errorf("upstream headers lost: %#v", record.ResponseHeaders)
		}
	}

	t.Run("stream", func(t *testing.T) { run(t, true) })
	t.Run("nonstream", func(t *testing.T) { run(t, false) })
}

func TestMetaExecutor_SubscriptionUsageFromJSONBody(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	observeMetaSubscriptionUsageBody(ctx, []byte(`{"object":"response","output":[],"subscription":{"window":{"used_percent":7,"resets_at":1794425416,"window_duration_mins":300},"weekly":{"used_percent":9,"resets_at":1794787200}}}`))
	headers := logging.GetResponseHeaders(ctx)
	if headers.Get("X-Muse-Subscription-Window-Used-Percent") != "7" || headers.Get("X-Muse-Subscription-Weekly-Used-Percent") != "9" {
		t.Fatalf("unexpected headers: %#v", headers)
	}
}
