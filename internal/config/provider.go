package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

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
// specified by the DNS_PROVIDER_PATH environment variable.
func LoadProviderConfig() (*ProviderConfig, error) {
	path := os.Getenv("DNS_PROVIDER_PATH")
	if path == "" {
		return nil, fmt.Errorf("DNS_PROVIDER_PATH environment variable is required")
	}
	return LoadProviderConfigFromPath(path)
}

// LoadProviderConfigFromPath reads the DNS provider configuration from the
// given file path.
func LoadProviderConfigFromPath(path string) (*ProviderConfig, error) {
	data, err := os.ReadFile(path) // #nosec G304,G703 -- path comes from an operator-set CLI flag, not request input
	if err != nil {
		return nil, fmt.Errorf("reading provider config file: %w", err)
	}

	// Unknown fields are an error, so a typo such as "upsrt: true" fails
	// startup instead of leaving the default in place.
	var cfg ProviderConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing provider config file: %w", err)
	}

	if cfg.Provider == "" {
		return nil, fmt.Errorf("provider config: missing required field 'provider'")
	}

	for k, v := range cfg.Settings {
		expanded, err := expandEnv(v)
		if err != nil {
			return nil, fmt.Errorf("provider config: settings.%s: %w", k, err)
		}
		cfg.Settings[k] = expanded
	}

	return &cfg, nil
}

// envRef matches a ${NAME} reference.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces each ${NAME} with the value of environment variable
// NAME. Nothing else is touched, so a literal "$" in a value survives, which
// os.ExpandEnv would not allow ("pa$word" became "pa"). A reference to an
// unset variable is an error rather than a silent empty string.
func expandEnv(value string) (string, error) {
	var missing []string
	expanded := envRef.ReplaceAllStringFunc(value, func(ref string) string {
		name := envRef.FindStringSubmatch(ref)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return expanded, nil
}
