package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const metaMuseCatalogFixture = `{"object":"list","data":[{"id":"muse-spark-1.3","object":"model","owned_by":"meta"}]}`

type metaMuseCatalogTransport func(*http.Request) (*http.Response, error)

func (transport metaMuseCatalogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func registerMetaMuseCatalogAuth(t *testing.T, server *Server, id, apiKey, baseURL string, subscribed, disabled bool) {
	t.Helper()
	attrs := map[string]string{"api_key": apiKey}
	if baseURL != "" {
		attrs["base_url"] = baseURL
	}
	credential := &auth.Auth{
		ID:         id,
		Provider:   "meta",
		Status:     auth.StatusActive,
		Disabled:   disabled,
		Attributes: attrs,
		Metadata:   map[string]any{"type": "meta", "is_subs_active": subscribed},
	}
	if disabled {
		credential.Status = auth.StatusDisabled
	}
	if _, errRegister := server.handlers.AuthManager.Register(t.Context(), credential); errRegister != nil {
		t.Fatalf("register Meta credential %s: %v", id, errRegister)
	}
}

func serveMetaMuseCatalog(t *testing.T, server *Server, apiKey string, transport metaMuseCatalogTransport) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	req := httptest.NewRequest(http.MethodGet, "/muse-code/models?client=muse", nil).WithContext(ctx)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	return recorder
}

func TestMetaMuseCodeModelsPassesCatalogThrough(t *testing.T) {
	server := newTestServer(t)
	server.handlers.AuthManager.RegisterExecutor(executor.NewMetaExecutor(server.getConfig()))
	registerMetaMuseCatalogAuth(t, server, "meta-free", "free-key", "", false, false)
	registerMetaMuseCatalogAuth(t, server, "meta-disabled", "disabled-key", "", true, true)
	registerMetaMuseCatalogAuth(t, server, "meta-sub", "sub-key", "https://api.meta.ai/v1/", true, false)

	var upstream []*http.Request
	recorder := serveMetaMuseCatalog(t, server, "test-key", func(req *http.Request) (*http.Response, error) {
		upstream = append(upstream, req.Clone(req.Context()))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(metaMuseCatalogFixture)),
			Request:    req,
		}, nil
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != metaMuseCatalogFixture {
		t.Fatalf("body = %q, want the upstream catalog unchanged", got)
	}
	if len(upstream) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(upstream))
	}
	got := upstream[0]
	if want := "https://api.meta.ai/muse-code/models?client=muse"; got.URL.String() != want {
		t.Errorf("upstream URL = %q, want %q", got.URL.String(), want)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer sub-key" {
		t.Errorf("Authorization = %q, want the subscribed credential's key", auth)
	}
	if ua := got.Header.Get("User-Agent"); !strings.HasPrefix(ua, "muse-build/") {
		t.Errorf("User-Agent = %q, want the Muse Code client identity", ua)
	}
	if id := got.Header.Get("X-Client-Id"); id == "" {
		t.Error("X-Client-Id missing")
	}
}

func TestMetaMuseCodeModelsFallsBackToNextCredential(t *testing.T) {
	server := newTestServer(t)
	server.handlers.AuthManager.RegisterExecutor(executor.NewMetaExecutor(server.getConfig()))
	registerMetaMuseCatalogAuth(t, server, "meta-a", "stale-key", "", true, false)
	registerMetaMuseCatalogAuth(t, server, "meta-b", "good-key", "", true, false)

	var keys []string
	recorder := serveMetaMuseCatalog(t, server, "test-key", func(req *http.Request) (*http.Response, error) {
		keys = append(keys, req.Header.Get("Authorization"))
		status, body := http.StatusOK, metaMuseCatalogFixture
		if req.Header.Get("Authorization") == "Bearer stale-key" {
			status, body = http.StatusUnauthorized, `{"error":"invalid key"}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(keys) != 2 || keys[0] != "Bearer stale-key" || keys[1] != "Bearer good-key" {
		t.Fatalf("credentials tried = %v, want stale-key then good-key", keys)
	}
}

func TestMetaMuseCodeModelsReportsLastUpstreamFailure(t *testing.T) {
	server := newTestServer(t)
	server.handlers.AuthManager.RegisterExecutor(executor.NewMetaExecutor(server.getConfig()))
	registerMetaMuseCatalogAuth(t, server, "meta-only", "bad-key", "", true, false)

	recorder := serveMetaMuseCatalog(t, server, "test-key", func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"forbidden"}`)),
			Request:    req,
		}, nil
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestMetaMuseCodeModelsRequiresProxyKeyAndCredential(t *testing.T) {
	server := newTestServer(t)
	server.handlers.AuthManager.RegisterExecutor(executor.NewMetaExecutor(server.getConfig()))
	noUpstream := func(req *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected upstream call to %s", req.URL)
		return nil, nil
	}

	if recorder := serveMetaMuseCatalog(t, server, "", noUpstream); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("without proxy key: status = %d, want 401", recorder.Code)
	}
	if recorder := serveMetaMuseCatalog(t, server, "test-key", noUpstream); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("without Meta credential: status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
}
