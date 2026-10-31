// Package providers lists the DNS backends and builds the one the config names.
package providers

import (
	"fmt"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/memory"
	opnsense "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
)

// providerFactory is a single provider candidate for initialization.
type providerFactory struct {
	name string
	// settings lists every key the provider understands, so a typo such as "sip_tls_verify" fails startup.
	settings []string
	fn       func(logr.Logger, map[string]string) (dns.Provider, error)
}

// newProviders lists the available providers, picked by name from the config; add entries here for a new backend.
func newProviders() []providerFactory {
	return []providerFactory{
		{
			name:     "opnsense",
			settings: []string{"base_url", "api_key", "api_secret", "ca_file", "skip_tls_verify", "allow_insecure_http", "default_ttl", "publish_interval"},
			fn: func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
				return opnsense.New(log, settings)
			},
		},
		{
			name:     "memory",
			settings: []string{"publish_interval"},
			fn: func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
				return memory.New(log, settings)
			},
		},
	}
}

// New initializes the named provider with its settings.
func New(log logr.Logger, name string, settings map[string]string) (dns.Provider, error) {
	return selectProvider(log, newProviders(), name, settings)
}

// selectProvider initializes exactly the named provider: a bad name or rejected settings is a config error, and the provider is returned only after the error check because a nil concrete pointer becomes a non-nil interface.
func selectProvider(log logr.Logger, factories []providerFactory, name string, settings map[string]string) (dns.Provider, error) {
	i := slices.IndexFunc(factories, func(pf providerFactory) bool { return pf.name == name })
	if i < 0 {
		var known []string
		for _, pf := range factories {
			known = append(known, pf.name)
		}
		return nil, fmt.Errorf("unknown DNS provider %q; available: %s", name, strings.Join(known, ", "))
	}
	pf := factories[i]
	var unknown []string
	for key := range settings {
		if !slices.Contains(pf.settings, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, fmt.Errorf("DNS provider %q: unknown settings %s; known settings: %s",
			name, strings.Join(unknown, ", "), strings.Join(pf.settings, ", "))
	}
	p, err := pf.fn(log.WithName("dns-"+pf.name), settings)
	if err != nil {
		return nil, fmt.Errorf("initializing DNS provider %q: %w", name, err)
	}
	return p, nil
}
