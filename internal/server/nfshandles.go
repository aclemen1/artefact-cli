package server

import (
	"crypto/sha256"
	"path"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// stableHandles gives every path of a view the same NFS handle for ever: the
// start of the SHA-256 of the path. Random handles kept in a bounded cache
// go stale when the cache evicts them (a view has more entries than it
// holds) and when serve restarts; these never do. The rest (directory
// verifiers) stays with go-nfs's caching handler.
type stableHandles struct {
	*nfshelper.CachingHandler
	fs    billy.Filesystem
	mu    sync.Mutex
	paths map[[16]byte][]string
}

func newStableHandles(fs billy.Filesystem) nfs.Handler {
	inner := nfshelper.NewCachingHandler(nfshelper.NewNullAuthHandler(fs), 4096).(*nfshelper.CachingHandler)
	return &stableHandles{CachingHandler: inner, fs: fs, paths: map[[16]byte][]string{}}
}

func handleOf(p []string) [16]byte {
	sum := sha256.Sum256([]byte(strings.Join(p, "/")))
	var h [16]byte
	copy(h[:], sum[:16])
	return h
}

func (s *stableHandles) ToHandle(_ billy.Filesystem, p []string) []byte {
	h := handleOf(p)
	s.mu.Lock()
	s.paths[h] = append([]string(nil), p...)
	s.mu.Unlock()
	return h[:]
}

func (s *stableHandles) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	if len(fh) != 16 {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
	}
	var h [16]byte
	copy(h[:], fh)
	s.mu.Lock()
	p, ok := s.paths[h]
	s.mu.Unlock()
	if ok {
		return s.fs, append([]string(nil), p...), nil
	}
	// A handle from before a restart: find its path by walking the view.
	if p, ok := s.find(h); ok {
		return s.fs, p, nil
	}
	return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
}

// find walks the view, recording every path, until one has handle h.
func (s *stableHandles) find(h [16]byte) ([]string, bool) {
	var found []string
	var walk func(p []string) bool
	walk = func(p []string) bool {
		k := handleOf(p)
		s.mu.Lock()
		s.paths[k] = p
		s.mu.Unlock()
		if k == h {
			found = p
			return true
		}
		entries, err := s.fs.ReadDir(path.Join(append([]string{"/"}, p...)...))
		if err != nil {
			return false
		}
		for _, e := range entries {
			child := append(append([]string(nil), p...), e.Name())
			if e.IsDir() {
				if walk(child) {
					return true
				}
				continue
			}
			k := handleOf(child)
			s.mu.Lock()
			s.paths[k] = child
			s.mu.Unlock()
			if k == h {
				found = child
				return true
			}
		}
		return false
	}
	walk([]string{})
	return found, found != nil
}

func (s *stableHandles) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

func (s *stableHandles) HandleLimit() int { return 1 << 30 }
