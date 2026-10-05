// Package backend copies files to the storage backends of a sphere through
// rclone (SPEC §6). A backend is an rclone remote path, e.g.
// "gdrive-perso:artefact", or an on-the-fly one such as ":local:/Volumes/X".
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/rclone/rclone/backend/drive"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/walk"
)

var setup sync.Once

// Init points rclone at artefact's own configuration file, so that its
// remotes and tokens stay apart from any other rclone setup.
func Init(configPath string) {
	setup.Do(func() {
		configfile.Install()
		_ = config.SetConfigPath(configPath)
		fs.GetConfig(context.Background()).LogLevel = fs.LogLevelError
	})
}

// ConfigPath is ~/.config/artefact/rclone.conf.
func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "artefact", "rclone.conf")
}

// Backend is one rclone remote path.
type Backend struct {
	Name string
	fs   fs.Fs
}

// Open connects to remote; root is the directory under which the sphere's
// files go, e.g. "gdrive-perso:artefact/perso".
func Open(ctx context.Context, name, root string) (*Backend, error) {
	f, err := fs.NewFs(ctx, root)
	if err != nil && !errors.Is(err, fs.ErrorIsFile) {
		return nil, fmt.Errorf("backend %s (%s): %w", name, root, err)
	}
	return &Backend{Name: name, fs: f}, nil
}

// Put writes r at p, replacing what is there.
func (b *Backend) Put(ctx context.Context, p string, r io.Reader, mod time.Time) error {
	_, err := operations.Rcat(ctx, b.fs, p, io.NopCloser(r), mod, nil)
	if err != nil {
		return fmt.Errorf("%s: put %s: %w", b.Name, p, err)
	}
	return nil
}

// Info is what the backend says of a file.
type Info struct {
	Size   int64
	SHA256 string // empty when the backend does not compute it
}

var ErrNotFound = errors.New("backend: not found")

func (b *Backend) Stat(ctx context.Context, p string) (Info, error) {
	o, err := b.fs.NewObject(ctx, p)
	if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) {
		return Info{}, ErrNotFound
	}
	if err != nil {
		return Info{}, err
	}
	in := Info{Size: o.Size()}
	if b.fs.Hashes().Contains(hash.SHA256) {
		in.SHA256, _ = o.Hash(ctx, hash.SHA256)
	}
	return in, nil
}

func (b *Backend) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	o, err := b.fs.NewObject(ctx, p)
	if err != nil {
		return nil, err
	}
	return o.Open(ctx)
}

// Delete removes the file p; an absent file is not an error.
func (b *Backend) Delete(ctx context.Context, p string) error {
	o, err := b.fs.NewObject(ctx, p)
	if errors.Is(err, fs.ErrorObjectNotFound) || errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return o.Remove(ctx)
}

// Purge removes the directory dir and all it holds; absent is not an error.
func (b *Backend) Purge(ctx context.Context, dir string) error {
	err := operations.Purge(ctx, b.fs, dir)
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil
	}
	return err
}

// List returns every file under dir, recursively, with paths relative to
// the backend's root.
func (b *Backend) List(ctx context.Context, dir string) ([]string, error) {
	var out []string
	err := walk.ListR(ctx, b.fs, dir, false, -1, walk.ListObjects, func(entries fs.DirEntries) error {
		for _, e := range entries {
			out = append(out, e.Remote())
		}
		return nil
	})
	if errors.Is(err, fs.ErrorDirNotFound) {
		return nil, nil
	}
	return out, err
}

// Join builds a backend path from slash-separated parts.
func Join(parts ...string) string { return strings.TrimPrefix(path.Join(parts...), "/") }

// CreateDrive runs rclone's Google Drive set-up for name: it opens the
// browser for the user's consent. The scope drive.file limits the remote to
// the files it creates; deletions skip the Drive trash.
func CreateDrive(ctx context.Context, name string) error {
	_, err := config.CreateRemote(ctx, name, "drive", map[string]any{
		"scope":     "drive.file",
		"use_trash": "false",
	}, config.UpdateRemoteOpt{})
	return err
}

// Remotes lists the remotes of artefact's rclone configuration.
func Remotes() []string { return config.FileSections() }
