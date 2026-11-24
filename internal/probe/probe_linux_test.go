//go:build linux

package probe

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupWorkingExcludesReclaimableCache(t *testing.T) {
	cgroupRoot = t.TempDir()
	dir := filepath.Join(cgroupRoot, "user.slice")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "memory.current"), []byte("42000000000\n"), 0o644)
	// 34G of file cache of which 2G is tmpfs and 1G dirty; 1G reclaimable slab.
	os.WriteFile(filepath.Join(dir, "memory.stat"), []byte("anon 4000000000\nfile 34000000000\nshmem 2000000000\n"+
		"file_dirty 1000000000\nfile_writeback 0\nslab_reclaimable 1000000000\n"), 0o644)
	got, ok := CgroupWorking("user.slice")
	want := int64(42000000000 - (34000000000 - 2000000000 - 1000000000) - 1000000000)
	if !ok || got != want {
		t.Fatalf("working %d, want %d (tmpfs and dirty pages stay counted)", got, want)
	}
}
