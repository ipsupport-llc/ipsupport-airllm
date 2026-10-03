package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"sync"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
)

// Every replica keeps the provider registry and a few settings rows in
// memory. A save through the admin API reloads them only on the replica that
// served it; the others pick the change up from the database on their next
// RefreshConfig. API keys and aliases are not here — the lookup cache already
// re-asks for them after its TTL.

// refreshedSettings are the settings rows held in memory, and how each is
// installed.
func (s *Server) settingAppliers() map[string]func([]byte) {
	return map[string]func([]byte){
		"dlp":        s.applyDLP,
		"capture":    s.applyCapture,
		"secondpass": s.applySecondpass,
		"failover":   s.applyFailover,
	}
}

// configState is what this instance last installed from the database.
type configState struct {
	mu        sync.Mutex // orders a local save against a concurrent refresh
	providers [sha256.Size]byte
	settings  map[string][]byte // raw value per settings row; absent = not read yet
}

// loadSetting reads one settings row and installs it. A missing or unreadable
// row installs the defaults.
func (s *Server) loadSetting(ctx context.Context, name string) {
	s.config.mu.Lock()
	defer s.config.mu.Unlock()
	raw, err := s.st.GetSetting(ctx, name)
	if err != nil {
		raw = nil
	} else {
		s.rememberSetting(name, raw)
	}
	s.settingAppliers()[name](raw)
}

func (s *Server) rememberSetting(name string, raw []byte) {
	if s.config.settings == nil {
		s.config.settings = map[string][]byte{}
	}
	s.config.settings[name] = raw
}

// reloadProviders rebuilds the registry from the DB (after a provider change).
func (s *Server) reloadProviders(ctx context.Context) error {
	s.config.mu.Lock()
	defer s.config.mu.Unlock()
	return s.loadProvidersLocked(ctx, true)
}

// loadProvidersLocked reads the provider rows and swaps in a registry built
// from them — unless force is false and they are the rows the current one was
// built from. Callers hold s.config.mu.
func (s *Server) loadProvidersLocked(ctx context.Context, force bool) error {
	rows, err := s.st.ListProvidersForRegistry(ctx)
	if err != nil {
		return err
	}
	fp := providers.Fingerprint(rows)
	if !force && fp == s.config.providers {
		return nil
	}
	s.regPtr.Store(providers.Build(ctx, rows, s.sealer))
	s.config.providers = fp
	if !force {
		slog.Info("providers reloaded from the database", "providers", len(rows))
	}
	return nil
}

// RefreshConfig re-reads the provider and settings rows this instance keeps in
// memory and installs whatever changed since it last looked. A read that
// fails changes nothing: the last good configuration keeps serving through a
// database outage.
func (s *Server) RefreshConfig(ctx context.Context) {
	s.config.mu.Lock()
	defer s.config.mu.Unlock()
	if err := s.loadProvidersLocked(ctx, false); err != nil {
		slog.Warn("config refresh: providers not read; keeping the current ones", "err", err)
	}
	appliers := s.settingAppliers()
	names := make([]string, 0, len(appliers))
	for name := range appliers {
		names = append(names, name)
	}
	raws, err := s.st.GetSettings(ctx, names)
	if err != nil {
		slog.Warn("config refresh: settings not read; keeping the current ones", "err", err)
		return
	}
	for name, apply := range appliers {
		raw := raws[name]
		if prev, seen := s.config.settings[name]; seen && bytes.Equal(prev, raw) {
			continue
		}
		apply(raw)
		s.rememberSetting(name, raw)
	}
}

// configRefreshTimeout bounds one refresh, so a database that hangs instead
// of refusing does not stall the next ones.
const configRefreshTimeout = 5 * time.Second

// StartConfigRefresh runs RefreshConfig every interval until ctx is cancelled.
func (s *Server) StartConfigRefresh(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, cancel := context.WithTimeout(ctx, configRefreshTimeout)
				s.RefreshConfig(rctx)
				cancel()
			}
		}
	}()
}
