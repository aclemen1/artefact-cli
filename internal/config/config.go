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
	Serve Serve             `yaml:"serve,omitempty"`
	Views Views             `yaml:"views,omitempty"`
	// Backends are the rclone remotes artefact copies objects to.
	Backends map[string]Backend `yaml:"backends,omitempty"`
	// Replication sets, per sphere, how many copies an artefact needs.
	Replication map[string]Replication `yaml:"replication,omitempty"`
	// Classify sets the deciders that place artefacts in the plans.
	Classify Classify `yaml:"classify,omitempty"`
	// RcloneConfig is rclone's configuration file; default ~/.config/artefact/rclone.conf.
	RcloneConfig string `yaml:"rclone_config,omitempty"`

	path string `yaml:"-"`
}

type Erase struct {
	CodeTTL string `yaml:"code_ttl,omitempty"` // validity of an erasure code, default 10m
}

type Serve struct {
	Socket string `yaml:"socket,omitempty"` // default ~/.local/state/artefact/sock
	HTTP   string `yaml:"http,omitempty"`   // loopback address, e.g. 127.0.0.1:7780
}

// Backend is one storage backend: an rclone remote path, owned by one sphere.
type Backend struct {
	Remote  string `yaml:"remote" json:"remote"`                       // e.g. gdrive-perso:artefact
	Sphere  string `yaml:"sphere" json:"sphere"`                       // the only sphere it may hold
	Trusted *bool  `yaml:"trusted,omitempty" json:"trusted,omitempty"` // default true; false: its copies do not count
}

func (b Backend) IsTrusted() bool { return b.Trusted == nil || *b.Trusted }

// Replication is the copy policy of a sphere. The local store is one copy.
type Replication struct {
	NumCopies int      `yaml:"numcopies" json:"numcopies"`
	MinCopies int      `yaml:"mincopies" json:"mincopies"`
	Backends  []string `yaml:"backends" json:"backends"`
	Every     string   `yaml:"every,omitempty" json:"every,omitempty"`         // how often serve syncs, default 15m
	Transfers int      `yaml:"transfers,omitempty" json:"transfers,omitempty"` // files sent at once, default 8
}

// ReplicationOf returns the policy of sphere, with defaults.
func (c *Config) ReplicationOf(sphere string) Replication {
	r := c.Replication[sphere]
	if r.NumCopies == 0 {
		r.NumCopies = 1 + len(r.Backends)
	}
	if r.MinCopies == 0 {
		r.MinCopies = 1
	}
	return r
}

// Classify configures classification (SPEC §9).
type Classify struct {
	Decider   string   `yaml:"decider,omitempty" json:"decider,omitempty"`     // rule, openjev, jev or acp; default openjev
	Threshold float64  `yaml:"threshold,omitempty" json:"threshold,omitempty"` // default 0.8
	Deciders  Deciders `yaml:"deciders,omitempty" json:"deciders,omitempty"`
}

type Deciders struct {
	OpenJev struct {
		URL   string `yaml:"url,omitempty" json:"url,omitempty"` // default http://127.0.0.1:3000
		Model string `yaml:"model,omitempty" json:"model,omitempty"`
	} `yaml:"openjev,omitempty" json:"openjev,omitempty"`
	Jev struct {
		URL   string `yaml:"url,omitempty" json:"url,omitempty"`     // default https://api.typesafe.ai
		Model string `yaml:"model,omitempty" json:"model,omitempty"` // default jev-latest
		// KeyCommand prints the TypeSafe API key, e.g. [security, find-generic-password, -s, typesafe-api-key, -w].
		KeyCommand []string `yaml:"key_command,omitempty" json:"key_command,omitempty"`
	} `yaml:"jev,omitempty" json:"jev,omitempty"`
	ACP struct {
		Command []string `yaml:"command,omitempty" json:"command,omitempty"` // e.g. [herdr-acp, --agent, claude]
	} `yaml:"acp,omitempty" json:"acp,omitempty"`
}

func (c *Config) ClassifyThreshold() float64 {
	if c.Classify.Threshold > 0 {
		return c.Classify.Threshold
	}
	return 0.8
}

// Views configures the read-only file views of `artefact serve`.
type Views struct {
	// NFS lists the spheres served over NFS; empty means every sphere.
	NFS []string `yaml:"nfs,omitempty"`
	// DisableNFS turns the NFS views off.
	DisableNFS bool `yaml:"disable_nfs,omitempty"`
	// Mounts maps a sphere to the directories serve mounts its view on: one
	// directory, or a list.
	Mounts map[string]Dirs `yaml:"mounts,omitempty"`
}

// Dirs is one directory or a list of them; YAML takes either form.
type Dirs []string

func (d *Dirs) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*d = Dirs{n.Value}
		return nil
	}
	var l []string
	if err := n.Decode(&l); err != nil {
		return err
	}
	*d = l
	return nil
}

// MarshalYAML writes a single directory as a plain string.
func (d Dirs) MarshalYAML() (any, error) {
	if len(d) == 1 {
		return d[0], nil
	}
	return []string(d), nil
}

// NFSSpheres are the spheres whose view serve exports over NFS.
func (c *Config) NFSSpheres() []string {
	if c.Views.DisableNFS {
		return nil
	}
	if len(c.Views.NFS) > 0 {
		return c.Views.NFS
	}
	return c.Spheres
}

// SocketPath is the Unix socket of `artefact serve`.
func (c *Config) SocketPath() string {
	if c.Serve.Socket != "" {
		return Expand(c.Serve.Socket)
	}
	return Expand("~/.local/state/artefact/sock")
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
	for name, b := range c.Backends {
		if b.Remote == "" || !slices.Contains(c.Spheres, b.Sphere) {
			return fmt.Errorf("configuration: backend %q needs a remote and one configured sphere, got remote %q sphere %q", name, b.Remote, b.Sphere)
		}
	}
	for sp, r := range c.Replication {
		for _, name := range r.Backends {
			b, ok := c.Backends[name]
			if !ok {
				return fmt.Errorf("configuration: replication of %q names an unknown backend %q", sp, name)
			}
			if b.Sphere != sp {
				return fmt.Errorf("configuration: backend %q belongs to sphere %q, not %q", name, b.Sphere, sp)
			}
		}
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
