package view

import (
	"errors"
	"io"
	"os"
	"path"

	"github.com/go-git/go-billy/v5"
)

var errReadOnly = errors.New("artefact view is read-only")

// Billy adapts a tree to billy.Filesystem, for the NFS server.
type Billy struct{ T *Tree }

var _ billy.Filesystem = Billy{}

func (b Billy) Create(string) (billy.File, error) { return nil, errReadOnly }
func (b Billy) Open(name string) (billy.File, error) {
	return b.OpenFile(name, os.O_RDONLY, 0)
}
func (b Billy) OpenFile(name string, flag int, _ os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errReadOnly
	}
	f, fi, err := b.T.Open(name)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return &dirFile{name: name, info: fi}, nil
	}
	return &file{name: name, f: f}, nil
}
func (b Billy) Stat(name string) (os.FileInfo, error)  { return b.T.Stat(name) }
func (b Billy) Lstat(name string) (os.FileInfo, error) { return b.T.Stat(name) }
func (b Billy) Rename(string, string) error            { return errReadOnly }
func (b Billy) Remove(string) error                    { return errReadOnly }
func (b Billy) Join(elem ...string) string             { return path.Join(elem...) }
func (b Billy) TempFile(string, string) (billy.File, error) {
	return nil, errReadOnly
}
func (b Billy) ReadDir(p string) ([]os.FileInfo, error) {
	l, err := b.T.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, len(l))
	for i, x := range l {
		out[i] = x
	}
	return out, nil
}
func (b Billy) MkdirAll(string, os.FileMode) error      { return errReadOnly }
func (b Billy) Symlink(string, string) error            { return errReadOnly }
func (b Billy) Readlink(string) (string, error)         { return "", errReadOnly }
func (b Billy) Chroot(string) (billy.Filesystem, error) { return nil, errReadOnly }
func (b Billy) Root() string                            { return "/" }
func (b Billy) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

type file struct {
	name string
	f    *os.File
}

func (f *file) Name() string                              { return f.name }
func (f *file) Read(p []byte) (int, error)                { return f.f.Read(p) }
func (f *file) ReadAt(p []byte, off int64) (int, error)   { return f.f.ReadAt(p, off) }
func (f *file) Seek(off int64, whence int) (int64, error) { return f.f.Seek(off, whence) }
func (f *file) Close() error                              { return f.f.Close() }
func (f *file) Write([]byte) (int, error)                 { return 0, errReadOnly }
func (f *file) Lock() error                               { return nil }
func (f *file) Unlock() error                             { return nil }
func (f *file) Truncate(int64) error                      { return errReadOnly }

type dirFile struct {
	name string
	info *Info
}

func (d *dirFile) Name() string                      { return d.name }
func (d *dirFile) Read([]byte) (int, error)          { return 0, io.EOF }
func (d *dirFile) ReadAt([]byte, int64) (int, error) { return 0, io.EOF }
func (d *dirFile) Seek(int64, int) (int64, error)    { return 0, nil }
func (d *dirFile) Close() error                      { return nil }
func (d *dirFile) Write([]byte) (int, error)         { return 0, errReadOnly }
func (d *dirFile) Lock() error                       { return nil }
func (d *dirFile) Unlock() error                     { return nil }
func (d *dirFile) Truncate(int64) error              { return errReadOnly }
