package app

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
)

func loadConfig(log logr.Logger, path string) (*config.Config, error) {
	if path == "" {
		return nil, errors.New("--config flag or CONFIG_PATH environment variable is required")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("unable to load config: %w", err)
	}
	log.Info("loaded config", "path", path, "provider", cfg.Provider, "upsert", cfg.Upsert)
	logDomainMap(log, cfg.DomainMap)
	return cfg, nil
}

// logDomainMap says what the controller will actually manage: a map that silently matches nothing is the usual way it appears to do nothing, so the count is info and the pairs debug.
func logDomainMap(log logr.Logger, domainMap *config.DomainMap) {
	entries := domainMap.Entries()
	log.Info("loaded domain map", "entries", len(entries))
	for _, d := range slices.Sorted(maps.Keys(entries)) {
		log.V(1).Info("domain map entry", "domain", d, "ip", entries[d])
		// exact keys used to cover every name below them too, so an upgrade with such a key releases the subdomains' records
		if !strings.HasPrefix(d, "*.") && entries["*."+d] == "" {
			log.Info("domain map key matches only this exact name; use \"*."+d+"\" to also cover the names below it", "domain", d)
		}
	}
}
