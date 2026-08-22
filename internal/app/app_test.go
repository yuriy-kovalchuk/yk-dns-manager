package app

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/config"
)

func testSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"API_KEY":    []byte("key123"),
			"API_SECRET": []byte("secret456"),
		},
	}
}

func TestLoadCredentials(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(testSecret("opnsense-creds", "xk"))

	// No secret referenced → nil, nil (provider that needs no credentials).
	creds, err := loadCredentials(ctx, client, "xk", "")
	if err != nil || creds != nil {
		t.Fatalf("expected (nil, nil) for empty secret name, got (%+v, %v)", creds, err)
	}

	// Existing secret → data passed through with the name for error messages.
	creds, err = loadCredentials(ctx, client, "xk", "opnsense-creds")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.SecretName != "opnsense-creds" {
		t.Errorf("expected SecretName 'opnsense-creds', got %q", creds.SecretName)
	}
	if creds.SecretKey("API_KEY") != "key123" {
		t.Errorf("expected API_KEY 'key123', got %q", creds.SecretKey("API_KEY"))
	}

	// Missing secret → error names the secret and namespace.
	_, err = loadCredentials(ctx, client, "xk", "nope")
	if err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "xk") {
		t.Errorf("error should name the secret and namespace, got: %v", err)
	}
}

func TestPodNamespaceFromEnv(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "from-env")
	ns, ok := podNamespace()
	if !ok || ns != "from-env" {
		t.Fatalf("expected (from-env, true), got (%q, %v)", ns, ok)
	}
}

func TestResolveSecretNamespace(t *testing.T) {
	// Explicit config value wins.
	ns, err := resolveSecretNamespace(&config.Config{SecretsNamespace: "explicit"})
	if err != nil || ns != "explicit" {
		t.Fatalf("expected (explicit, nil), got (%q, %v)", ns, err)
	}
	// No secrets referenced → empty namespace is fine.
	ns, err = resolveSecretNamespace(&config.Config{
		Providers: map[string]config.ProviderInstance{"a": {}},
	})
	if err != nil || ns != "" {
		t.Fatalf("expected ('', nil) when no secret is referenced, got (%q, %v)", ns, err)
	}
}

func TestBuild_NoProviders(t *testing.T) {
	ctx := context.Background()
	m, err := Build(ctx, logr.Discard(), &config.Config{DomainMap: config.NewDomainMap(nil)}, fake.NewClientset())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Len() != 0 {
		t.Fatalf("expected empty manager, got %d instances", m.Len())
	}
}

func TestBuild_UnknownProviderType(t *testing.T) {
	cfg := &config.Config{
		DomainMap: config.NewDomainMap(nil),
		Providers: map[string]config.ProviderInstance{
			"bad": {Provider: "nope", Secret: "s", Settings: map[string]string{"base_url": "https://x/api"}},
		},
		SecretsNamespace: "xk",
	}
	if _, err := Build(context.Background(), logr.Discard(), cfg, fake.NewClientset()); err == nil {
		t.Fatal("expected error for unknown provider type, got nil")
	}
}

func TestBuild_MissingSecret(t *testing.T) {
	cfg := &config.Config{
		DomainMap: config.NewDomainMap(nil),
		Providers: map[string]config.ProviderInstance{
			"op": {Provider: "opnsense", Secret: "nope", Settings: map[string]string{"base_url": "https://x/api"}},
		},
		SecretsNamespace: "xk",
	}
	if _, err := Build(context.Background(), logr.Discard(), cfg, fake.NewClientset()); err == nil {
		t.Fatal("expected error for missing secret, got nil")
	}
}

func TestBuild_ProviderNeedsSecret(t *testing.T) {
	cfg := &config.Config{
		DomainMap: config.NewDomainMap(nil),
		Providers: map[string]config.ProviderInstance{
			// No secret configured — opnsense requires one.
			"op": {Provider: "opnsense", Settings: map[string]string{"base_url": "https://x/api"}},
		},
	}
	_, err := Build(context.Background(), logr.Discard(), cfg, fake.NewClientset())
	if err == nil {
		t.Fatal("expected error when opnsense instance has no secret, got nil")
	}
}
