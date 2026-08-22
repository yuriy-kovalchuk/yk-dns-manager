// Package app assembles the runtime dependencies of yk-dns-manager:
// provider instances (with credentials read from the Kubernetes API) and
// the DNS manager. The main package only parses flags and starts the
// controller manager.
package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
	dns "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
	opnsense "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
)

// providerFactory constructs one DNS provider instance from its non-secret
// settings and credentials (nil when no secret is configured). Each
// provider validates the credential keys it expects; instance-name context
// is added by the caller's error wrapping.
type providerFactory func(log logr.Logger, settings map[string]string, creds *dns.Credentials) (dns.Provider, error)

// factories is the registry of supported provider types, keyed by type
// name. New provider types are added here:
//
//	"pihole": func(log logr.Logger, settings map[string]string, creds *dns.Credentials) (dns.Provider, error) {
//	    return pihole.New(log, settings, creds)
//	},
var factories = map[string]providerFactory{
	"opnsense": func(log logr.Logger, settings map[string]string, creds *dns.Credentials) (dns.Provider, error) {
		return opnsense.New(log, settings, creds)
	},
}

// Build creates the DNS manager from config: it resolves the credential
// secret namespace, reads each instance's Secret via the Kubernetes API,
// instantiates the providers, and health-checks them. Every configured
// instance must initialize successfully. A zero-instance config yields an
// empty manager (no-op mode).
func Build(ctx context.Context, log logr.Logger, cfg *config.Config, kube kubernetes.Interface) (*dns.Manager, error) {
	secretNamespace, err := resolveSecretNamespace(cfg)
	if err != nil {
		return nil, err
	}

	manager := dns.NewManager(log)
	for name, inst := range cfg.Providers {
		providerType := inst.Provider
		if providerType == "" {
			providerType = name
		}
		factory, ok := factories[providerType]
		if !ok {
			return nil, fmt.Errorf("provider instance %q: unknown provider type %q", name, providerType)
		}

		var creds *dns.Credentials
		if inst.Secret != "" {
			creds, err = loadCredentials(ctx, kube, secretNamespace, inst.Secret)
			if err != nil {
				return nil, fmt.Errorf("provider instance %q: %w", name, err)
			}
		}

		log.Info("initializing provider instance", "name", name, "type", providerType, "secret", inst.Secret)
		p, err := factory(log.WithName("dns-"+name), inst.Settings, creds)
		if err != nil {
			return nil, fmt.Errorf("provider instance %q: %w", name, err)
		}
		manager.Add(name, inst.Upsert, p)
	}

	if manager.Len() == 0 {
		log.Info("no DNS provider instances configured — running in no-op mode (HTTPRoutes are watched but no DNS records are managed)")
		return manager, nil
	}
	log.Info("checking DNS provider connectivity")
	if err := manager.HealthCheck(ctx); err != nil {
		return nil, fmt.Errorf("DNS provider health check failed: %w", err)
	}
	return manager, nil
}

// resolveSecretNamespace returns the namespace provider credential Secrets
// are read from: the explicit config value, else the pod's own namespace
// (in-cluster default).
func resolveSecretNamespace(cfg *config.Config) (string, error) {
	if cfg.SecretsNamespace != "" {
		return cfg.SecretsNamespace, nil
	}
	needsSecrets := false
	for _, inst := range cfg.Providers {
		if inst.Secret != "" {
			needsSecrets = true
			break
		}
	}
	if !needsSecrets {
		return "", nil
	}
	ns, ok := podNamespace()
	if !ok {
		return "", fmt.Errorf("provider instances reference credential secrets but no namespace could be resolved — run in-cluster or set 'secretsNamespace' in the config file")
	}
	return ns, nil
}

// podNamespace returns the namespace the app runs in: the POD_NAMESPACE
// env var (downward API) or the serviceaccount mount. The second is always
// present in-cluster, so this also serves as an "are we in a cluster" probe.
func podNamespace() (string, bool) {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns, true
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// loadCredentials resolves the credential Secret for one provider instance
// via the Kubernetes API. Returns nil when the instance references no
// secret (providers that need no credentials).
func loadCredentials(ctx context.Context, client kubernetes.Interface, namespace, secretName string) (*dns.Credentials, error) {
	if secretName == "" {
		return nil, nil
	}
	sec, err := client.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading secret %q in namespace %q: %w", secretName, namespace, err)
	}
	return &dns.Credentials{SecretName: secretName, Data: sec.Data}, nil
}
