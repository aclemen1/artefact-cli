package ocfl

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SourceFile is one logical file of a rebuilt version. Path is an existing
// file: a content file of the old object, or a temporary file.
type SourceFile struct {
	Logical string
	Path    string
}

// NewVersion describes one version of a rebuilt object.
type NewVersion struct {
	Files   []SourceFile
	Created time.Time
	Message string
	User    *User
}

// Rebuild replaces the object at objDir by a new object made of versions,
// numbered v1…vn in that order. Content files are hard-linked when they
// already live on the same volume. The object's extensions/ directory is
// carried over. The swap leaves either the old or the new object in place:
// an interruption between its two renames is undone by Recover.
// Files that only the old object held are deleted with it.
func Rebuild(root, objDir string, versions []NewVersion) (*Inventory, error) {
	if len(versions) == 0 {
		return nil, fmt.Errorf("ocfl: an object needs at least one version")
	}
	old, err := ReadInventory(objDir)
	if err != nil {
		return nil, err
	}
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
	inv := &Inventory{
		ID: old.ID, Type: inventoryType, DigestAlgorithm: "sha512", ContentDirectory: "content",
		Fixity:   map[string]map[string][]string{"sha256": {}},
		Manifest: map[string][]string{},
		Versions: map[string]*Version{},
	}
	for i, v := range versions {
		name := fmt.Sprintf("v%d", i+1)
		ver := &Version{Created: v.Created.Format(time.RFC3339), Message: v.Message, User: v.User, State: map[string][]string{}}
		for _, f := range v.Files {
			d512, d256, err := digests(f.Path)
			if err != nil {
				return nil, err
			}
			if _, seen := inv.Manifest[d512]; !seen {
				content := name + "/content/" + f.Logical
				dst := filepath.Join(dir, filepath.FromSlash(content))
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return nil, err
				}
				if err := linkOrCopy(f.Path, dst); err != nil {
					return nil, err
				}
				inv.Manifest[d512] = []string{content}
				inv.Fixity["sha256"][d256] = append(inv.Fixity["sha256"][d256], content)
			}
			ver.State[d512] = append(ver.State[d512], f.Logical)
		}
		inv.Versions[name] = ver
		inv.Head = name
	}
	if err := os.WriteFile(filepath.Join(dir, objectNamaste), []byte("ocfl_object_1.1\n"), 0o644); err != nil {
		return nil, err
	}
	for _, name := range inv.VersionNames() {
		vdir := filepath.Join(dir, name)
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			return nil, err
		}
		// Each version directory keeps the inventory as it stood at that
		// version, so earlier versions stay self-describing.
		vinv := inv.upTo(name)
		if name == inv.Head {
			vinv = inv
		}
		if err := writeInventory(vdir, vinv); err != nil {
			return nil, err
		}
	}
	if err := writeInventory(dir, inv); err != nil {
		return nil, err
	}
	if err := copyDir(filepath.Join(objDir, "extensions"), filepath.Join(dir, "extensions")); err != nil {
		return nil, err
	}
	if err := swap(root, objDir, dir); err != nil {
		return nil, err
	}
	ok = true
	return inv, nil
}

// upTo returns the inventory as it stood at version v: the versions up to v
// and the content they use.
func (inv *Inventory) upTo(v string) *Inventory {
	out := &Inventory{ID: inv.ID, Type: inv.Type, DigestAlgorithm: inv.DigestAlgorithm, Head: v, ContentDirectory: inv.ContentDirectory,
		Fixity: map[string]map[string][]string{"sha256": {}}, Manifest: map[string][]string{}, Versions: map[string]*Version{}}
	used := map[string]bool{}
	for _, name := range inv.VersionNames() {
		out.Versions[name] = inv.Versions[name]
		for d := range inv.Versions[name].State {
			used[d] = true
		}
		if name == v {
			break
		}
	}
	paths := map[string]bool{}
	for d := range used {
		out.Manifest[d] = inv.Manifest[d]
		for _, p := range inv.Manifest[d] {
			paths[p] = true
		}
	}
	for d, ps := range inv.Fixity["sha256"] {
		for _, p := range ps {
			if paths[p] {
				out.Fixity["sha256"][d] = append(out.Fixity["sha256"][d], p)
			}
		}
	}
	return out
}

const (
	trashPrefix = "trash-"
	staleAfter  = 24 * time.Hour
)

// swap replaces objDir by newDir. The old object goes to the staging area
// with a note of where it came from, then is deleted.
func swap(root, objDir, newDir string) error {
	trash := filepath.Join(root, stagingDir, trashPrefix+RandomHex(8))
	rel, err := filepath.Rel(root, objDir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(trash+".target", []byte(rel), 0o644); err != nil {
		return err
	}
	if err := os.Rename(objDir, trash); err != nil {
		os.Remove(trash + ".target")
		return err
	}
	if err := os.Rename(newDir, objDir); err != nil {
		_ = os.Rename(trash, objDir)
		os.Remove(trash + ".target")
		return err
	}
	return purge(trash)
}

// Remove deletes the object at objDir for good.
func Remove(root, objDir string) error {
	trash := filepath.Join(root, stagingDir, trashPrefix+RandomHex(8))
	if err := os.MkdirAll(filepath.Dir(trash), 0o755); err != nil {
		return err
	}
	if err := os.Rename(objDir, trash); err != nil {
		return err
	}
	return purge(trash)
}

func purge(trash string) error {
	_ = makeWritable(trash)
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	if err := os.Remove(trash + ".target"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Recover undoes what an interrupted rebuild left in the staging area: an
// old object whose replacement never arrived is put back, a replaced one is
// deleted. Other leftovers are deleted once stale. Call it under the
// sphere's write lock.
func Recover(root string) error {
	dir := filepath.Join(root, stagingDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".target") {
			continue
		}
		if strings.HasPrefix(e.Name(), trashPrefix) {
			if rel, err := os.ReadFile(p + ".target"); err == nil {
				dst := filepath.Join(root, string(rel))
				if _, err := os.Stat(dst); os.IsNotExist(err) {
					if err := os.Rename(p, dst); err != nil {
						return err
					}
					os.Remove(p + ".target")
					continue
				}
			}
		} else if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < staleAfter {
			// Another process may be staging it right now.
			continue
		}
		if err := purge(p); err != nil {
			return err
		}
	}
	// Notes whose trash is gone.
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".target") {
			if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(e.Name(), ".target"))); os.IsNotExist(err) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil
}

func digests(path string) (d512, d256 string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h512, h256 := sha512.New(), sha256.New()
	if _, err := io.Copy(io.MultiWriter(h512, h256), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(h512.Sum(nil)), hex.EncodeToString(h256.Sum(nil)), nil
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if os.IsNotExist(err) && p == src {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// ContentPath is the absolute path of a content file of the object.
func ContentPath(objDir string, f File) string {
	return filepath.Join(objDir, filepath.FromSlash(f.Content))
}
