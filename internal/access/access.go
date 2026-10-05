// Package access keeps the bearer tokens of remote callers (SPEC §12). A
// token is shown once; only its SHA-256 is stored.
package access

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

var Rights = []string{"read", "write", "reveal", "erase"}

type Token struct {
	Name    string   `json:"name"`
	Hash    string   `json:"hash"`
	Spheres []string `json:"spheres"`
	Rights  []string `json:"rights"`
	Created string   `json:"created"`
}

// Public is a token without its hash, for listings.
type Public struct {
	Name    string   `json:"name"`
	Spheres []string `json:"spheres"`
	Rights  []string `json:"rights"`
	Created string   `json:"created"`
}

func (t Token) Public() Public { return Public{t.Name, t.Spheres, t.Rights, t.Created} }

type Store struct {
	path string
	mu   sync.Mutex
}

func Open(stateDir string) *Store { return &Store{path: filepath.Join(stateDir, "tokens.json")} }

func (s *Store) load() ([]Token, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Token{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Token
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("tokens %s: %w", s.path, err)
	}
	return out, nil
}

func (s *Store) save(ts []Token) error {
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Create makes a token and returns its secret, shown this once.
func (s *Store) Create(name string, spheres, rights []string) (string, Public, error) {
	if name == "" {
		return "", Public{}, fmt.Errorf("--name names the client. Example: artefact token create --name mnemo --spheres perso,pro --rights read,write,reveal,erase")
	}
	if len(spheres) == 0 {
		return "", Public{}, fmt.Errorf("--spheres lists the spheres granted. Example: --spheres perso")
	}
	for _, r := range rights {
		if !slices.Contains(Rights, r) {
			return "", Public{}, fmt.Errorf("unknown right %q; rights: %s", r, strings.Join(Rights, ", "))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return "", Public{}, err
	}
	for _, t := range ts {
		if t.Name == name {
			return "", Public{}, fmt.Errorf("a token named %q exists; revoke it first or choose another name", name)
		}
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	secret := "art_" + base64.RawURLEncoding.EncodeToString(b)
	t := Token{Name: name, Hash: hash(secret), Spheres: spheres, Rights: rights, Created: time.Now().Format(time.RFC3339)}
	if err := s.save(append(ts, t)); err != nil {
		return "", Public{}, err
	}
	return secret, t.Public(), nil
}

func (s *Store) List() ([]Public, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []Public{}
	for _, t := range ts {
		out = append(out, t.Public())
	}
	return out, nil
}

// Revoke removes the token named name and reports whether it existed.
func (s *Store) Revoke(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return false, err
	}
	n := len(ts)
	ts = slices.DeleteFunc(ts, func(t Token) bool { return t.Name == name })
	if len(ts) == n {
		return false, nil
	}
	return true, s.save(ts)
}

// Verify returns the token whose secret this is.
func (s *Store) Verify(secret string) (*Token, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, err := s.load()
	if err != nil {
		return nil, false
	}
	h := hash(strings.TrimSpace(secret))
	for _, t := range ts {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			return &t, true
		}
	}
	return nil, false
}
