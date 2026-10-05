// Package config reads ~/.config/artefact/config.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Store         string   `yaml:"store"`
	Spheres       []string `yaml:"spheres"`
	DefaultSphere string   `yaml:"default_sphere"`
	// Keys maps a sphere to its age recipient (public key).
	Keys  map[string]string `yaml:"keys,omitempty"`
	Erase Erase             `yaml:"erase,omitempty"`

	path string `yaml:"-"`
}

type Erase struct {
	CodeTTL string `yaml:"code_ttl,omitempty"` // validity of an erasure code, default 10m
}

// CodeTTL is the validity of an erasure code.
func (c *Config) CodeTTL() time.Duration {
	if d, err := time.ParseDuration(c.Erase.CodeTTL); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// Path is the file the configuration was read from.
func (c *Config) Path() string { return c.path }

// DefaultPath is ~/.config/artefact/config.yaml, or $ARTEFACT_CONFIG.
func DefaultPath() string {
	if p := os.Getenv("ARTEFACT_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "artefact", "config.yaml")
}

func Default() *Config {
	return &Config{Store: "~/artefact", Spheres: []string{"perso"}, DefaultSphere: "perso"}
}

// Load reads the configuration at path (DefaultPath when empty).
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	b, err := os.ReadFile(Expand(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no configuration at %s. Run `artefact init` first", path)
		}
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("configuration %s: %w", path, err)
	}
	c.path = path
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.Store == "" {
		return fmt.Errorf("configuration: `store` is empty")
	}
	if len(c.Spheres) == 0 {
		return fmt.Errorf("configuration: `spheres` is empty")
	}
	for _, s := range c.Spheres {
		if !ValidSphere(s) {
			return fmt.Errorf("configuration: sphere %q must be lower-case letters, digits and dashes", s)
		}
	}
	if c.DefaultSphere == "" {
		c.DefaultSphere = c.Spheres[0]
	}
	if !slices.Contains(c.Spheres, c.DefaultSphere) {
		return fmt.Errorf("configuration: default_sphere %q is not in spheres %v", c.DefaultSphere, c.Spheres)
	}
	return nil
}

// Save writes the configuration at path (DefaultPath when empty).
func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultPath()
	}
	path = Expand(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func (c *Config) StorePath() string { return Expand(c.Store) }

func ValidSphere(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Expand replaces a leading ~ by the home directory.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
