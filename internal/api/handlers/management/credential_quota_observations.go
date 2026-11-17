package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// GetCredentialQuotaObservations returns in-memory quota snapshots without querying providers.
func (h *Handler) GetCredentialQuotaObservations(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	files := make([]gin.H, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil || auth.Quota.ObservedAt.IsZero() || len(auth.Quota.Signals) == 0 {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider != "codex" && provider != "claude" {
			continue
		}
		auth.EnsureIndex()
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = auth.ID
		}
		files = append(files, gin.H{
			"id": auth.ID, "name": name, "auth_index": auth.Index, "provider": provider, "type": provider,
			"quota": quotaObservationPayloadForProvider(provider, auth.Quota),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i]["auth_index"].(string) < files[j]["auth_index"].(string)
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"observed_at": time.Now().UTC(), "files": files})
}
