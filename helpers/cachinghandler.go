package helpers

import (
	"crypto/sha256"
	"encoding/binary"
	"io/fs"
	"reflect"
	"sync"

	"github.com/willscott/go-nfs"

	"github.com/go-git/go-billy/v5"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
)

// NewCachingHandler wraps a handler to provide a basic to/from-file handle cache.
func NewCachingHandler(h nfs.Handler, limit int) nfs.Handler {
	return NewCachingHandlerWithVerifierLimit(h, limit, limit)
}

// NewCachingHandlerWithVerifierLimit provides a basic to/from-file handle cache that can be tuned with a smaller cache of active directory listings.
func NewCachingHandlerWithVerifierLimit(h nfs.Handler, limit int, verifierLimit int) nfs.Handler {
	if limit < 2 || verifierLimit < 2 {
		nfs.Log.Warnf("Caching handler created with insufficient cache to support directory listing", "size", limit, "verifiers", verifierLimit)
	}
	cache, _ := lru.New[uuid.UUID, entry](limit)
	verifiers, _ := lru.New[uint64, verifier](verifierLimit)
	return &CachingHandler{
		Handler:         h,
		activeHandles:   cache,
		reverseHandles:  make(map[string][]uuid.UUID),
		activeVerifiers: verifiers,
		cacheLimit:      limit,
	}
}

// CachingHandler implements to/from handle via an LRU cache.
type CachingHandler struct {
	nfs.Handler
	activeHandles    *lru.Cache[uuid.UUID, entry]
	reverseHandles   map[string][]uuid.UUID
	reverseHandlesMu sync.RWMutex
	activeVerifiers  *lru.Cache[uint64, verifier]
	cacheLimit       int
}

type entry struct {
	f billy.Filesystem
	p []string
}

// ToHandle takes a file and represents it with an opaque handle to reference it.
// In stateless nfs (when it's serving a unix fs) this can be the device + inode
// but we can generalize with a stateful local cache of handed out IDs.
func (c *CachingHandler) ToHandle(f billy.Filesystem, path []string) []byte {
	joinedPath := f.Join(path...)

	if handle := c.searchReverseCache(f, joinedPath); handle != nil {
		return handle
	}

	id := uuid.New()

	newPath := make([]string, len(path))

	copy(newPath, path)
	evictedKey, evictedPath, ok := c.activeHandles.GetOldest()
	if evicted := c.activeHandles.Add(id, entry{f, newPath}); evicted && ok {
		rk := evictedPath.f.Join(evictedPath.p...)
		c.evictReverseCache(rk, evictedKey)
	}

	c.appendReverseHandle(joinedPath, id)
	b, _ := id.MarshalBinary()

	return b
}

// FromHandle converts from an opaque handle to the file it represents
func (c *CachingHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	id, err := uuid.FromBytes(fh)
	if err != nil {
		return nil, []string{}, err
	}

	if f, ok := c.activeHandles.Get(id); ok {
		// Keep this handle's ancestor directory handles warm so an active
		// child can't outlive its parent in the LRU (ESTALE on the parent).
		// Walk the O(depth) ancestors via the reverse map instead of scanning
		// the whole handle cache: the previous Keys() scan was O(N) per call,
		// which made a READDIRPLUS walk of an N-entry directory O(N^2) and
		// allocated an N-sized slice on every RPC.
		for i := len(f.p) - 1; i >= 0; i-- {
			for _, aid := range c.getReverseHandles(f.f.Join(f.p[:i]...)) {
				_, _ = c.activeHandles.Get(aid)
			}
		}
		newP := make([]string, len(f.p))
		copy(newP, f.p)
		return f.f, newP, nil
	}
	return nil, []string{}, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
}

// UpdateHandle remaps an existing handle to a new path so it keeps resolving
// after a rename (NFS file handles must survive renames).
func (c *CachingHandler) UpdateHandle(f billy.Filesystem, handle []byte, newPath []string) error {
	id, err := uuid.FromBytes(handle)
	if err != nil {
		return err
	}
	np := make([]string, len(newPath))
	copy(np, newPath)
	c.activeHandles.Add(id, entry{f, np})
	c.appendReverseHandle(f.Join(np...), id)
	return nil
}

func (c *CachingHandler) searchReverseCache(f billy.Filesystem, path string) []byte {
	uuids := c.getReverseHandles(path)

	for _, id := range uuids {
		if candidate, ok := c.activeHandles.Get(id); ok {
			if reflect.DeepEqual(candidate.f, f) {
				return id[:]
			}
		}
	}

	return nil
}

func (c *CachingHandler) evictReverseCache(path string, handle uuid.UUID) {
	c.reverseHandlesMu.Lock()
	defer c.reverseHandlesMu.Unlock()

	uuids, ok := c.reverseHandles[path]
	if !ok {
		return
	}
	for i, u := range uuids {
		if u == handle {
			c.reverseHandles[path] = append(uuids[:i], uuids[i+1:]...)
			return
		}
	}
}

func (c *CachingHandler) getReverseHandles(path string) []uuid.UUID {
	c.reverseHandlesMu.RLock()
	defer c.reverseHandlesMu.RUnlock()
	return c.reverseHandles[path]
}

func (c *CachingHandler) appendReverseHandle(path string, id uuid.UUID) {
	c.reverseHandlesMu.Lock()
	defer c.reverseHandlesMu.Unlock()
	c.reverseHandles[path] = append(c.reverseHandles[path], id)
}

func (c *CachingHandler) InvalidateHandle(fs billy.Filesystem, handle []byte) error {
	//Remove from cache
	id, _ := uuid.FromBytes(handle)
	entry, ok := c.activeHandles.Get(id)
	if ok {
		rk := entry.f.Join(entry.p...)
		c.evictReverseCache(rk, id)
	}
	c.activeHandles.Remove(id)
	return nil
}

// HandleLimit exports how many file handles can be safely stored by this cache.
func (c *CachingHandler) HandleLimit() int {
	return c.cacheLimit
}

func hasPrefix(path, prefix []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i, e := range prefix {
		if path[i] != e {
			return false
		}
	}
	return true
}

type verifier struct {
	path     string
	contents []fs.FileInfo
}

func hashPathAndContents(path string, contents []fs.FileInfo) uint64 {
	//calculate a cookie-verifier.
	vHash := sha256.New()

	// Add the path to avoid collisions of directories with the same content
	vHash.Write(binary.BigEndian.AppendUint64([]byte{}, uint64(len(path))))
	vHash.Write([]byte(path))

	for _, c := range contents {
		vHash.Write([]byte(c.Name())) // Never fails according to the docs
	}

	verify := vHash.Sum(nil)[0:8]
	return binary.BigEndian.Uint64(verify)
}

func (c *CachingHandler) VerifierFor(path string, contents []fs.FileInfo) uint64 {
	id := hashPathAndContents(path, contents)
	c.activeVerifiers.Add(id, verifier{path, contents})
	return id
}

func (c *CachingHandler) DataForVerifier(path string, id uint64) []fs.FileInfo {
	if cache, ok := c.activeVerifiers.Get(id); ok {
		return cache.contents
	}
	return nil
}
