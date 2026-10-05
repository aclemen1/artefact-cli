// Package ocfl writes and reads the subset of OCFL 1.1 that artefact uses:
// a storage root with the 0004 hashed n-tuple layout, and objects whose
// versions hold whole files.
package ocfl

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	rootNamaste   = "0=ocfl_1.1"
	objectNamaste = "0=ocfl_object_1.1"
	inventoryName = "inventory.json"
	layoutName    = "0004-hashed-n-tuple-storage-layout"
	inventoryType = "https://ocfl.io/1.1/spec/#inventory"
	stagingDir    = "extensions/artefact-staging"
)

var ErrExists = errors.New("ocfl: object already exists")

// InitRoot makes dir an OCFL storage root. It is idempotent.
func InitRoot(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "extensions", layoutName), 0o755); err != nil {
		return err
	}
	files := map[string]string{
		rootNamaste:        "ocfl_1.1\n",
		"ocfl_layout.json": `{"extension": "` + layoutName + `", "description": "Objects stored under the SHA-256 of their id, in three tuples of three characters."}` + "\n",
		filepath.Join("extensions", layoutName, "config.json"): `{"extensionName": "` + layoutName + `", "digestAlgorithm": "sha256", "tupleSize": 3, "numberOfTuples": 3, "shortObjectRoot": false}` + "\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// IsRoot reports whether dir is an OCFL storage root.
func IsRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, rootNamaste))
	return err == nil
}

// ObjectPath is the object root of id, relative to the storage root.
func ObjectPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(h[0:3], h[3:6], h[6:9], h)
}

type User struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}

type Version struct {
	Created string              `json:"created"`
	State   map[string][]string `json:"state"`
	Message string              `json:"message,omitempty"`
	User    *User               `json:"user,omitempty"`
}

type Inventory struct {
	ID               string                         `json:"id"`
	Type             string                         `json:"type"`
	DigestAlgorithm  string                         `json:"digestAlgorithm"`
	Head             string                         `json:"head"`
	ContentDirectory string                         `json:"contentDirectory,omitempty"`
	Fixity           map[string]map[string][]string `json:"fixity,omitempty"`
	Manifest         map[string][]string            `json:"manifest"`
	Versions         map[string]*Version            `json:"versions"`
}

// File is one logical file of a version.
type File struct {
	Logical string // path inside the version's state, e.g. payload.pdf
	Content string // path inside the object, e.g. v1/content/payload.pdf
	SHA512  string
	SHA256  string
}

// Files lists the logical files of version v ("" for the head).
func (inv *Inventory) Files(v string) ([]File, error) {
	if v == "" {
		v = inv.Head
	}
	ver, ok := inv.Versions[v]
	if !ok {
		return nil, fmt.Errorf("ocfl: no version %s in %s (head %s)", v, inv.ID, inv.Head)
	}
	sha256Of := map[string]string{}
	for d, paths := range inv.Fixity["sha256"] {
		for _, p := range paths {
			sha256Of[p] = d
		}
	}
	var out []File
	for d, logicals := range ver.State {
		contents := inv.Manifest[d]
		if len(contents) == 0 {
			return nil, fmt.Errorf("ocfl: digest %s of %s is missing from the manifest", d, inv.ID)
		}
		for _, l := range logicals {
			out = append(out, File{Logical: l, Content: contents[0], SHA512: d, SHA256: sha256Of[contents[0]]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Logical < out[j].Logical })
	return out, nil
}

// VersionNames returns v1…vn in order.
func (inv *Inventory) VersionNames() []string {
	var out []string
	for i := 1; ; i++ {
		v := fmt.Sprintf("v%d", i)
		if _, ok := inv.Versions[v]; !ok {
			return out
		}
		out = append(out, v)
	}
}

// ReadInventory reads and checks the inventory of the object at dir.
func ReadInventory(dir string) (*Inventory, error) {
	b, err := os.ReadFile(filepath.Join(dir, inventoryName))
	if err != nil {
		return nil, err
	}
	side, err := os.ReadFile(filepath.Join(dir, inventoryName+".sha512"))
	if err != nil {
		return nil, err
	}
	sum := sha512.Sum512(b)
	want, _, _ := strings.Cut(strings.TrimSpace(string(side)), " ")
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("ocfl: inventory of %s does not match its sidecar", dir)
	}
	var inv Inventory
	if err := json.Unmarshal(b, &inv); err != nil {
		return nil, fmt.Errorf("ocfl: inventory of %s: %w", dir, err)
	}
	return &inv, nil
}

// Staged is a new object written in the storage root's staging area,
// waiting to be committed into place.
type Staged struct {
	Dir    string // staging directory, the future object root
	Inv    *Inventory
	SHA256 string
	SHA512 string
	Size   int64
}

// Stage writes a new object with one version holding one file, read from r.
func Stage(root, id, logical string, r io.Reader, created time.Time, message string, user *User) (*Staged, error) {
	if err := os.MkdirAll(filepath.Join(root, stagingDir), 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(root, stagingDir), "obj-")
	if err != nil {
		return nil, err
	}
	s := &Staged{Dir: dir}
	ok := false
	defer func() {
		if !ok {
			s.Abort()
		}
	}()
	content := filepath.Join("v1", "content", logical)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, content)), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, content), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o444)
	if err != nil {
		return nil, err
	}
	h512, h256 := sha512.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h512, h256), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	s.Size, s.SHA512, s.SHA256 = n, hex.EncodeToString(h512.Sum(nil)), hex.EncodeToString(h256.Sum(nil))
	content = filepath.ToSlash(content)
	s.Inv = &Inventory{
		ID: id, Type: inventoryType, DigestAlgorithm: "sha512", Head: "v1", ContentDirectory: "content",
		Fixity:   map[string]map[string][]string{"sha256": {s.SHA256: {content}}},
		Manifest: map[string][]string{s.SHA512: {content}},
		Versions: map[string]*Version{"v1": {
			Created: created.Format(time.RFC3339), Message: message, User: user,
			State: map[string][]string{s.SHA512: {logical}},
		}},
	}
	if err := os.WriteFile(filepath.Join(dir, objectNamaste), []byte("ocfl_object_1.1\n"), 0o644); err != nil {
		return nil, err
	}
	for _, d := range []string{dir, filepath.Join(dir, "v1")} {
		if err := writeInventory(d, s.Inv); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

// Commit moves the staged object to its place under root. The rename is
// atomic: an interrupted commit leaves either nothing or the whole object.
func (s *Staged) Commit(root string) (string, error) {
	rel := ObjectPath(s.Inv.ID)
	dst := filepath.Join(root, rel)
	if _, err := os.Stat(dst); err == nil {
		s.Abort()
		return "", ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(s.Dir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (s *Staged) Abort() {
	if s.Dir != "" {
		_ = makeWritable(s.Dir)
		_ = os.RemoveAll(s.Dir)
	}
}

func writeInventory(dir string, inv *Inventory) error {
	b, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	sum := sha512.Sum512(b)
	if err := os.WriteFile(filepath.Join(dir, inventoryName), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, inventoryName+".sha512"), []byte(hex.EncodeToString(sum[:])+"  "+inventoryName+"\n"), 0o644)
}

// Walk calls fn with the directory of every object under root.
func Walk(root string, fn func(objDir string) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && d.Name() == "extensions" && filepath.Dir(p) == root {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(p, objectNamaste)); err == nil {
			if err := fn(p); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		return nil
	})
}

// Verify recomputes the SHA-512 of every content file of the object.
func Verify(dir string) error {
	inv, err := ReadInventory(dir)
	if err != nil {
		return err
	}
	for d, paths := range inv.Manifest {
		for _, p := range paths {
			f, err := os.Open(filepath.Join(dir, filepath.FromSlash(p)))
			if err != nil {
				return err
			}
			h := sha512.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
			if hex.EncodeToString(h.Sum(nil)) != d {
				return fmt.Errorf("ocfl: %s in %s does not match its digest", p, inv.ID)
			}
		}
	}
	return nil
}

// RandomHex returns n random bytes in hex, for temporary names.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func makeWritable(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
}
