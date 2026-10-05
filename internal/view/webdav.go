package view

import (
	"context"
	"io"
	"io/fs"
	"os"

	"golang.org/x/net/webdav"
)

// DAV adapts a tree to webdav.FileSystem.
type DAV struct{ T *Tree }

var _ webdav.FileSystem = DAV{}

func (d DAV) Mkdir(context.Context, string, os.FileMode) error { return errReadOnly }
func (d DAV) RemoveAll(context.Context, string) error          { return errReadOnly }
func (d DAV) Rename(context.Context, string, string) error     { return errReadOnly }
func (d DAV) Stat(_ context.Context, name string) (os.FileInfo, error) {
	return d.T.Stat(name)
}
func (d DAV) OpenFile(_ context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errReadOnly
	}
	f, fi, err := d.T.Open(name)
	if err != nil {
		return nil, err
	}
	return &davFile{t: d.T, name: name, f: f, info: fi}, nil
}

type davFile struct {
	t    *Tree
	name string
	f    *os.File // nil for a directory
	info *Info
	read bool
}

func (d *davFile) Close() error {
	if d.f != nil {
		return d.f.Close()
	}
	return nil
}
func (d *davFile) Read(p []byte) (int, error) {
	if d.f == nil {
		return 0, io.EOF
	}
	return d.f.Read(p)
}
func (d *davFile) Seek(off int64, whence int) (int64, error) {
	if d.f == nil {
		return 0, nil
	}
	return d.f.Seek(off, whence)
}
func (d *davFile) Write([]byte) (int, error)  { return 0, errReadOnly }
func (d *davFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *davFile) Readdir(count int) ([]fs.FileInfo, error) {
	if d.f != nil {
		return nil, errReadOnly
	}
	if d.read {
		if count > 0 {
			return nil, io.EOF
		}
		return nil, nil
	}
	d.read = true
	l, err := d.t.ReadDir(d.name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.FileInfo, len(l))
	for i, x := range l {
		out[i] = x
	}
	return out, nil
}
