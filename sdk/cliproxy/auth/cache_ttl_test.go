package auth

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"testing"

	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestRequestCacheTTL(t *testing.T) {
	tests := []struct{ name, payload, want string }{
		{"tools", `{"tools":[{"cache_control":{"ttl":"1h"}}]}`, "1h"},
		{"system", `{"system":[{"cache_control":{}},{"cache_control":{"ttl":"1h"}}]}`, "1h"},
		{"messages", `{"tools":[{"cache_control":{"ttl":"5m"}}],"messages":[{"content":[{"cache_control":{"ttl":"1h"}}]}]}`, "1h"},
		{"default", `{"messages":[{"content":[{"cache_control":{}}]}]}`, "5m"},
		{"automatic", `{"tools":[{"cache_control":{"ttl":"5m"}}],"cache_control":{"ttl":"1h"}}`, "1h"},
		{"none", `{"messages":[]}`, ""},
		{"strings", `{"system":"text","messages":[{"content":"text"}]}`, ""},
		{"invalid", `{`, ""},
		{"unknown last", `{"tools":[{"cache_control":{}}],"cache_control":{"ttl":"2h"}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestCacheTTL([]byte(tt.payload)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCacheTTLReachesSchedulerOptions(t *testing.T) {
	original := map[string]any{"other": "value"}
	opts := withRequestCacheTTL(executor.Request{Payload: []byte(`{"cache_control":{}}`)}, executor.Options{Metadata: original, OriginalRequest: []byte(`{"cache_control":{"ttl":"1h"}}`)})
	got := schedulerOptions(opts)
	if got.Metadata[executor.CacheTTLMetadataKey] != "1h" {
		t.Fatalf("metadata: %#v", got.Metadata)
	}
	if _, ok := original[executor.CacheTTLMetadataKey]; ok {
		t.Fatal("caller metadata mutated")
	}
	got.Metadata[executor.CacheTTLMetadataKey] = "5m"
	if opts.Metadata[executor.CacheTTLMetadataKey] != "1h" {
		t.Fatal("scheduler metadata was not cloned")
	}
}

type cacheTTLSelector struct{ got any }

func (s *cacheTTLSelector) Pick(_ context.Context, _, _ string, opts executor.Options, auths []*Auth) (*Auth, error) {
	s.got = schedulerOptions(opts).Metadata[executor.CacheTTLMetadataKey]
	return auths[0], nil
}

func TestExecuteCacheTTLReachesSelection(t *testing.T) {
	ctx := context.Background()
	selector := &cacheTTLSelector{}
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex"})
	auth := &Auth{ID: "cache-ttl-selection", Provider: "codex", Status: StatusActive}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "cache-ttl-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	_, _ = manager.Execute(ctx, []string{"codex"}, executor.Request{Model: "cache-ttl-model", Payload: []byte(`{"cache_control":{"ttl":"1h"}}`)}, executor.Options{})
	if selector.got != "1h" {
		t.Fatalf("selector metadata: %v", selector.got)
	}
}
