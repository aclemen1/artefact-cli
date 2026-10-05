package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
	"golang.org/x/net/webdav"

	"github.com/aclemen1/artefact-cli/internal/config"
	"github.com/aclemen1/artefact-cli/internal/spec"
	"github.com/aclemen1/artefact-cli/internal/store"
	"github.com/aclemen1/artefact-cli/internal/view"
)

// Views holds the trees of the spheres, shared by NFS and WebDAV.
type Views struct {
	st    *store.Store
	trees map[string]*view.Tree
	dav   map[string]*webdav.Handler
}

func newViews(cfg *config.Config) (*Views, error) {
	st, err := store.Open(cfg, nil, "view")
	if err != nil {
		return nil, err
	}
	v := &Views{st: st, trees: map[string]*view.Tree{}, dav: map[string]*webdav.Handler{}}
	for _, sp := range cfg.Spheres {
		// Opening every index now keeps later concurrent reads lock-free.
		if _, err := st.ViewRows(sp); err != nil {
			st.Close()
			return nil, err
		}
		t := view.New(st, sp)
		v.trees[sp] = t
		v.dav[sp] = &webdav.Handler{Prefix: "/dav/" + sp, FileSystem: view.DAV{T: t}, LockSystem: webdav.NewMemLS()}
	}
	return v, nil
}

func (v *Views) Close() { v.st.Close() }

// serveDAV serves /dav/<sphere>/…, read-only, within the caller's spheres.
func (h *Server) serveDAV(w http.ResponseWriter, r *http.Request, sp string) {
	a := accessOf(r)
	if h.views == nil {
		writeError(w, spec.NotFound("no file views on this server"))
		return
	}
	d, ok := h.views.dav[sp]
	if !ok || !slices.Contains(a.Spheres, sp) || !a.Can("read") {
		writeError(w, spec.NotFound("no view of sphere %q for this caller", sp))
		return
	}
	switch r.Method {
	case "GET", "HEAD", "OPTIONS", "PROPFIND", "LOCK", "UNLOCK":
		d.ServeHTTP(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS, PROPFIND")
		http.Error(w, "the artefact view is read-only", http.StatusMethodNotAllowed)
	}
}

// NFSState is what serve publishes about its NFS views: one port per sphere.
type NFSState map[string]int

func nfsStatePath(cfg *config.Config) string {
	return filepath.Join(cfg.StorePath(), ".artefact", "nfs.json")
}

// ReadNFSState reads the ports of the running serve.
func ReadNFSState(cfg *config.Config) (NFSState, error) {
	b, err := os.ReadFile(nfsStatePath(cfg))
	if err != nil {
		return nil, fmt.Errorf("no NFS view running: start `artefact serve` first")
	}
	var s NFSState
	return s, json.Unmarshal(b, &s)
}

// startNFS listens on loopback for each exported sphere.
func (h *Server) startNFS(errc chan<- error) ([]net.Listener, error) {
	state := NFSState{}
	var ls []net.Listener
	for _, sp := range h.Cfg.NFSSpheres() {
		t, ok := h.views.trees[sp]
		if !ok {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, x := range ls {
				x.Close()
			}
			return nil, err
		}
		ls = append(ls, l)
		state[sp] = l.Addr().(*net.TCPAddr).Port
		handler := nfshelper.NewCachingHandler(nfshelper.NewNullAuthHandler(view.Billy{T: t}), 4096)
		go func() { errc <- nfs.Serve(l, handler) }()
	}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(nfsStatePath(h.Cfg), b, 0o600); err != nil {
		return ls, err
	}
	return ls, nil
}

// MountOptions are the NFS options of a view: read-only, soft so that a
// stopped serve never hangs the processes that read the mount.
func MountOptions(port int) string {
	p := strconv.Itoa(port)
	return "port=" + p + ",mountport=" + p + ",vers=3,tcp,nolocks,rdonly,soft,intr,timeo=10,retrans=2,nobrowse"
}

// Mount mounts the NFS view of sphere on dir.
func Mount(cfg *config.Config, sphere, dir string) error {
	state, err := ReadNFSState(cfg)
	if err != nil {
		return err
	}
	port, ok := state[sphere]
	if !ok {
		return fmt.Errorf("serve exports no NFS view of sphere %q", sphere)
	}
	dir = config.Expand(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if mounted(dir) {
		return nil
	}
	out, err := exec.Command("/sbin/mount", "-t", "nfs", "-o", MountOptions(port), "127.0.0.1:/", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s: %s", dir, strings.TrimSpace(string(out)))
	}
	return nil
}

// Unmount unmounts dir if a view is mounted there.
func Unmount(dir string) error {
	dir = config.Expand(dir)
	if !mounted(dir) {
		return nil
	}
	out, err := exec.Command("/sbin/umount", dir).CombinedOutput()
	if err != nil {
		if out2, err2 := exec.Command("/sbin/umount", "-f", dir).CombinedOutput(); err2 != nil {
			return fmt.Errorf("umount %s: %s / %s", dir, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

func mounted(dir string) bool {
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		abs = dir
	}
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " on "+abs+" (") || strings.Contains(line, " on "+dir+" (") {
			return true
		}
	}
	return false
}
