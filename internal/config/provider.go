package config

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// ProviderConfig holds the DNS provider type, app-level options, and
// provider-specific connection settings.
type ProviderConfig struct {
	Provider string            `yaml:"provider"`
	Upsert   bool              `yaml:"upsert"`
	Settings map[string]string `yaml:"settings"`
}

// LoadProviderConfig reads the DNS provider configuration from the path
// specified by the DNS_PROVIDER_PATH environment variable, defaulting to
// "configs/dns-provider.yaml".
func LoadProviderConfig() (*ProviderConfig, error) {
	path := os.Getenv("DNS_PROVIDER_PATH")
	if path == "" {
		path = "configs/dns-provider.yaml"
	}
	return LoadProviderConfigFromPath(path)
}

// LoadProviderConfigFromPath reads the DNS provider configuration from the
// given file path.
func LoadProviderConfigFromPath(path string) (*ProviderConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading provider config file: %w", err)
	}

	var cfg ProviderConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing provider config file: %w", err)
	}

	if cfg.Provider == "" {
		return nil, fmt.Errorf("provider config: missing required field 'provider'")
	}

	// Expand ${ENV_VAR} references in setting values.
	for k, v := range cfg.Settings {
		cfg.Settings[k] = os.ExpandEnv(v)
	}

	return &cfg, nil
}
