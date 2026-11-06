package api

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const metaMuseCatalogMaxBytes = 8 << 20

// metaMuseCodeModels answers the Muse Code CLI's startup catalog request
// (GET /muse-code/models). Muse Code refuses to start without it. The catalog
// depends on the account's plan, so it is fetched from Meta with a configured
// Meta credential instead of being built from the proxy's model registry.
// Credentials are tried in order (active subscriptions first) until one answers 2xx.
func (s *Server) metaMuseCodeModels(c *gin.Context) {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Meta auth manager unavailable"})
		return
	}
	candidates := metaMuseCatalogCandidates(s.handlers.AuthManager.List())
	if len(candidates) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no Meta credential available for the Muse Code model catalog"})
		return
	}
	ctx := context.WithValue(c.Request.Context(), "gin", c)
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	lastStatus := http.StatusBadGateway
	lastError := "Muse Code model catalog request failed"
	for _, current := range candidates {
		upstreamURL := executor.MetaMuseCodeURL(current, "/muse-code/models")
		if c.Request.URL.RawQuery != "" {
			upstreamURL += "?" + c.Request.URL.RawQuery
		}
		req, errRequest := s.handlers.AuthManager.NewHttpRequest(ctx, current, http.MethodGet, upstreamURL, nil, headers)
		if errRequest != nil {
			lastError = errRequest.Error()
			continue
		}
		resp, errDo := s.handlers.AuthManager.HttpRequest(ctx, current, req)
		if errDo != nil {
			log.WithError(errDo).WithField("auth_id", current.ID).Warn("meta muse catalog: upstream request failed")
			lastError = errDo.Error()
			continue
		}
		body, errRead := io.ReadAll(io.LimitReader(resp.Body, metaMuseCatalogMaxBytes))
		_ = resp.Body.Close()
		if errRead != nil {
			lastError = errRead.Error()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.WithField("auth_id", current.ID).Warnf("meta muse catalog: upstream status %d", resp.StatusCode)
			lastStatus = resp.StatusCode
			lastError = strings.TrimSpace(string(body))
			continue
		}
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(resp.StatusCode, contentType, body)
		return
	}
	if lastError == "" {
		lastError = http.StatusText(lastStatus)
	}
	c.JSON(lastStatus, gin.H{"error": lastError})
}

// metaMuseCatalogCandidates lists enabled Meta credentials, active
// subscriptions first, then by ID so the choice is stable.
func metaMuseCatalogCandidates(all []*auth.Auth) []*auth.Auth {
	out := make([]*auth.Auth, 0, len(all))
	for _, a := range all {
		if a == nil || a.Disabled || a.Status == auth.StatusDisabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.Provider), "meta") {
			continue
		}
		out = append(out, a)
	}
	subscribed := func(a *auth.Auth) bool {
		active, _ := a.Metadata["is_subs_active"].(bool)
		return active
	}
	sort.SliceStable(out, func(i, j int) bool {
		if si, sj := subscribed(out[i]), subscribed(out[j]); si != sj {
			return si
		}
		return out[i].ID < out[j].ID
	})
	return out
}
