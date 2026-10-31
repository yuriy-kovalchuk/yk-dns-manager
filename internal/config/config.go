// Package config loads the controller's configuration file: the DNS provider and the domain map.
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

// Config is the controller's configuration: which DNS provider to use, how, and which IP each domain resolves to.
type Config struct {
	Provider  string
	Upsert    bool
	DomainMap *DomainMap
	Settings  map[string]string
}

// Load reads the configuration file at path, validating everything at startup rather than at reconcile time.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from an operator-set flag, not request input
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// unknown fields are an error, so a typo such as "upsrt: true" fails startup instead of leaving the default in place
	var raw struct {
		Provider string            `yaml:"provider"`
		Upsert   bool              `yaml:"upsert"`
		Domains  map[string]string `yaml:"domains"`
		Settings map[string]string `yaml:"settings"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if raw.Provider == "" {
		return nil, fmt.Errorf("config file %s: missing required field 'provider'", path)
	}
	domainMap, err := NewDomainMap(raw.Domains)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	for k, v := range raw.Settings {
		expanded, err := expandEnv(v)
		if err != nil {
			return nil, fmt.Errorf("config file %s: settings.%s: %w", path, k, err)
		}
		raw.Settings[k] = expanded
	}

	return &Config{Provider: raw.Provider, Upsert: raw.Upsert, DomainMap: domainMap, Settings: raw.Settings}, nil
}

// envRef matches a ${NAME} reference.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces each ${NAME} with an environment value and nothing else, so a literal "$" survives where os.ExpandEnv would eat it; an unset variable is an error.
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
