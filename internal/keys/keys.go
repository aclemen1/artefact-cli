// Package keys holds the age identities of the spheres: public key in the
// configuration, private key in the macOS keychain (SPEC §4.3).
package keys

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"filippo.io/age"
)

const service = "artefact"

func account(sphere string) string { return "age-" + sphere }

// Source gives the private identity of a sphere.
type Source interface {
	Identity(sphere string) (string, error)
}

// EnvVar names the variable that may hold a sphere's identity instead of
// the keychain, for tests and throwaway stores: ARTEFACT_AGE_IDENTITY_<SPHERE>,
// upper-cased, dashes as underscores.
func EnvVar(sphere string) string {
	return "ARTEFACT_AGE_IDENTITY_" + strings.ToUpper(strings.ReplaceAll(sphere, "-", "_"))
}

// Keychain reads identities from $ARTEFACT_AGE_IDENTITY_<SPHERE> when set,
// otherwise from the login keychain.
type Keychain struct{}

func (Keychain) Identity(sphere string) (string, error) {
	if id := strings.TrimSpace(os.Getenv(EnvVar(sphere))); id != "" {
		return id, nil
	}
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account(sphere), "-w").Output()
	if err != nil {
		return "", fmt.Errorf("no age key for sphere %q in the keychain (service %s, account %s). Run `artefact key init --sphere %s`", sphere, service, account(sphere), sphere)
	}
	return strings.TrimSpace(string(out)), nil
}

// Store keeps a sphere's identity in the login keychain.
func Store(sphere, identity string) error {
	return exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", account(sphere), "-w", identity).Run()
}

// Memory is a Source for tests.
type Memory map[string]string

func (m Memory) Identity(sphere string) (string, error) {
	if id, ok := m[sphere]; ok {
		return id, nil
	}
	return "", fmt.Errorf("no age key for sphere %q", sphere)
}

// Generate returns a new identity and its recipient.
func Generate() (identity, recipient string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

// Recipient derives the public key of an identity.
func Recipient(identity string) (string, error) {
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}

// Encrypt wraps w so that what is written to it is sealed for recipient.
func Encrypt(w io.Writer, recipient string) (io.WriteCloser, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("age recipient: %w", err)
	}
	return age.Encrypt(w, r)
}

// Decrypt opens a sealed stream with identity.
func Decrypt(r io.Reader, identity string) (io.Reader, error) {
	id, err := age.ParseX25519Identity(identity)
	if err != nil {
		return nil, err
	}
	return age.Decrypt(r, id)
}

// DedupKey is the HMAC key that names a sealed payload without revealing
// it: derived from the identity, so it never leaves the keychain's reach.
func DedupKey(identity string) []byte {
	h := sha256.Sum256([]byte("artefact dedup v1\x00" + identity))
	return h[:]
}

// Keyed returns the keyed digest of plaintext read through the returned writer.
func Keyed(key []byte) (io.Writer, func() string) {
	m := hmac.New(sha256.New, key)
	return m, func() string { return "hmac-sha256:" + hex.EncodeToString(m.Sum(nil)) }
}
