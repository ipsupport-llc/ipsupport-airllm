package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// modelCatalogTTL bounds staleness of the upstream model list micro-cache.
const modelCatalogTTL = 5 * time.Minute

// modelCatalogTimeout bounds a single upstream list-models call.
const modelCatalogTimeout = 10 * time.Second

type catalogEntry struct {
	models    []string
	fetchedAt time.Time
}

// catalogCache is a tiny TTL cache keyed by provider name. The zero value is
// ready to use.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]catalogEntry
}

func (c *catalogCache) get(name string, now time.Time) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok || now.Sub(e.fetchedAt) > modelCatalogTTL {
		return nil, false
	}
	return e.models, true
}

func (c *catalogCache) put(name string, models []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]catalogEntry{}
	}
	c.entries[name] = catalogEntry{models: models, fetchedAt: now}
}

// handleAdminProviderModels proxies the upstream model list for one provider
// so the alias editor can offer real model ids instead of free-text guessing.
// Only successful fetches are cached (TTL modelCatalogTTL); errors always
// retry upstream.
func (s *Server) handleAdminProviderModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	entry, ok := s.reg().Get(name)
	if !ok {
		writeControlError(w, http.StatusNotFound, "provider not found")
		return
	}
	lister, ok := entry.Provider.(providers.ModelLister)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"models": []string{}, "unsupported": true})
		return
	}
	if models, ok := s.catalog.get(name, time.Now()); ok {
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelCatalogTimeout)
	defer cancel()
	models, err := lister.ListModels(ctx)
	if err != nil {
		writeControlError(w, http.StatusBadGateway, "upstream list models: "+err.Error())
		return
	}
	if models == nil {
		models = []string{}
	}
	s.catalog.put(name, models, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
