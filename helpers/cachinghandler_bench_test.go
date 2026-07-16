package helpers

import (
	"fmt"
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/willscott/go-nfs"
)

// benchReaddirWalk simulates what a READDIRPLUS walk of an n-entry directory
// does to the handle cache: ToHandle once per entry (the per-entry handle in
// each readDirPlusEntity), plus two FromHandle calls per page RPC
// (onReadDirPlus itself and getDirListingWithVerifier).
func benchReaddirWalk(b *testing.B, n, pageSize int) {
	fs := memfs.New()
	for b.Loop() {
		h := NewCachingHandler(nfs.Handler(nil), 1_000_000).(*CachingHandler)
		dir := []string{"mnt", "hammerspace", "lakehouse", "images"}
		dirHandle := h.ToHandle(fs, dir)
		for i := 0; i < n; i++ {
			if i%pageSize == 0 { // one page RPC
				if _, _, err := h.FromHandle(dirHandle); err != nil {
					b.Fatal(err)
				}
				if _, _, err := h.FromHandle(dirHandle); err != nil {
					b.Fatal(err)
				}
			}
			h.ToHandle(fs, append(dir[:len(dir):len(dir)], fmt.Sprintf("img_%07d.jpg", i)))
		}
	}
}

func BenchmarkReaddirWalk10k(b *testing.B)  { benchReaddirWalk(b, 10_000, 128) }
func BenchmarkReaddirWalk50k(b *testing.B)  { benchReaddirWalk(b, 50_000, 128) }
func BenchmarkReaddirWalk208k(b *testing.B) { benchReaddirWalk(b, 208_459, 128) }
