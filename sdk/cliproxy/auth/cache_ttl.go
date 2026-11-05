package auth

import (
	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// requestCacheTTL returns the lifetime of the last breakpoint in prompt order.
func requestCacheTTL(payload []byte) string {
	if !ojson.ValidBytes(payload) {
		return ""
	}
	root := ojson.ParseBytes(payload)
	ttl := ""
	check := func(block ojson.Result) {
		cc := block.Get("cache_control")
		if !cc.IsObject() {
			return
		}
		value := cc.Get("ttl")
		if !value.Exists() {
			ttl = "5m"
		} else if value.Type == ojson.String && (value.String() == "5m" || value.String() == "1h") {
			ttl = value.String()
		} else {
			ttl = ""
		}
	}
	for _, tool := range root.Get("tools").Array() {
		check(tool)
	}
	for _, block := range root.Get("system").Array() {
		check(block)
	}
	for _, message := range root.Get("messages").Array() {
		for _, block := range message.Get("content").Array() {
			check(block)
		}
	}
	check(root)
	return ttl
}

func withRequestCacheTTL(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	payload := opts.OriginalRequest
	if len(payload) == 0 {
		payload = req.Payload
	}
	ttl := requestCacheTTL(payload)
	_, exists := opts.Metadata[cliproxyexecutor.CacheTTLMetadataKey]
	if ttl == "" && !exists {
		return opts
	}
	metadata := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	if ttl == "" {
		delete(metadata, cliproxyexecutor.CacheTTLMetadataKey)
	} else {
		metadata[cliproxyexecutor.CacheTTLMetadataKey] = ttl
	}
	opts.Metadata = metadata
	return opts
}
