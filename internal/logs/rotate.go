package logs

import (
	"fmt"
	"os"
	"sync"
)

// RotatingFile is a size-rotated, compressed log for the daemon itself when it
// does not run under journald: path, path.1.gz ... path.<keep>.gz.
type RotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func OpenRotating(path string, max int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{path: path, max: max, keep: keep}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	info, _ := f.Stat()
	r.f = f
	if info != nil {
		r.size = info.Size()
	}
	return r, nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotate() {
	r.f.Close()
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d.gz", r.path, i), fmt.Sprintf("%s.%d.gz", r.path, i+1))
	}
	_ = os.Remove(fmt.Sprintf("%s.%d.gz", r.path, r.keep+1))
	if _, err := compress(r.path, r.path+".1.gz"); err == nil {
		_ = os.Remove(r.path)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		r.f, r.size = f, 0
	}
}
