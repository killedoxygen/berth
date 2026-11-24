// Package logs stores job output compactly and bounded.
//
// A job's output is one logical byte stream written into segments of at most
// SegmentSize bytes. The live segment is plain text (so it can be followed);
// every full segment is gzip-compressed. Segment file names carry the stream
// offsets they cover, so offsets stay meaningful after segments are dropped:
//
//	<id>.<start>-<end>.log.gz   finished segment, stream bytes [start, end)
//	<id>.<start>.log            live segment, from start to start+size
//
// When a job's compressed segments exceed MaxBytes, the oldest segment after
// the first is dropped: the head (how it started) and the tail (how it ended)
// are kept, and readers see a marker where output was dropped.
package logs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/killedoxygen/berth/internal/units"
)

// Writer appends a job's output. It is safe for concurrent writes.
type Writer struct {
	mu       sync.Mutex
	dir, id  string
	segSize  int64
	maxBytes int64
	cur      *os.File
	curStart int64
	curLen   int64
	done     []segment // compressed, oldest first
	err      error
}

type segment struct {
	start, end int64
	path       string
	size       int64 // bytes on disk
	live       bool
}

// Create starts a job's log. segSize and maxBytes <= 0 mean 4M and 64M.
func Create(dir, id string, segSize, maxBytes int64) (*Writer, error) {
	if segSize <= 0 {
		segSize = 4 * units.MiB
	}
	if maxBytes <= 0 {
		maxBytes = 64 * units.MiB
	}
	w := &Writer{dir: dir, id: id, segSize: segSize, maxBytes: maxBytes}
	return w, w.open(0)
}

func (w *Writer) open(start int64) error {
	f, err := os.OpenFile(filepath.Join(w.dir, fmt.Sprintf("%s.%d.log", w.id, start)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w.cur, w.curStart, w.curLen = f, start, 0
	return nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		room := w.segSize - w.curLen
		chunk := p
		if int64(len(chunk)) > room {
			// Prefer to cut at a line end so segments hold whole lines.
			cut := room
			if i := bytes.LastIndexByte(chunk[:room], '\n'); i >= 0 && int64(i) > room/2 {
				cut = int64(i) + 1
			}
			chunk = chunk[:cut]
		}
		n, err := w.cur.Write(chunk)
		written += n
		w.curLen += int64(n)
		if err != nil {
			w.err = err
			return written, err
		}
		p = p[n:]
		if w.curLen >= w.segSize {
			if err := w.rotate(true); err != nil {
				w.err = err
				return written, err
			}
		}
	}
	return written, nil
}

// rotate compresses the live segment and, if another follows, opens it.
func (w *Writer) rotate(next bool) error {
	livePath := w.cur.Name()
	if err := w.cur.Close(); err != nil {
		return err
	}
	end := w.curStart + w.curLen
	if w.curLen > 0 {
		gzPath := filepath.Join(w.dir, fmt.Sprintf("%s.%d-%d.log.gz", w.id, w.curStart, end))
		size, err := compress(livePath, gzPath)
		if err != nil {
			return err
		}
		w.done = append(w.done, segment{start: w.curStart, end: end, path: gzPath, size: size})
	}
	if err := os.Remove(livePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	w.trim()
	if next {
		return w.open(end)
	}
	return nil
}

// trim drops the oldest segments after the first until within maxBytes.
func (w *Writer) trim() {
	total := int64(0)
	for _, s := range w.done {
		total += s.size
	}
	for total > w.maxBytes && len(w.done) > 2 {
		drop := w.done[1]
		_ = os.Remove(drop.path)
		total -= drop.size
		w.done = append(w.done[:1], w.done[2:]...)
	}
}

// Close compresses the last segment: finished logs are fully compressed.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return w.err
	}
	err := w.rotate(false)
	w.cur = nil
	if w.err == nil {
		w.err = err
	}
	return err
}

func compress(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if _, err := io.Copy(zw, bufio.NewReaderSize(in, 256<<10)); err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, err
	}
	info, err := out.Stat()
	if err == nil {
		err = out.Close()
	} else {
		out.Close()
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return info.Size(), os.Rename(tmp, dst)
}

// ---------------------------------------------------------------- reading

// segments lists a job's segments in stream order.
func segments(dir, id string) []segment {
	matches, _ := filepath.Glob(filepath.Join(dir, id+".*.log*"))
	if _, err := os.Stat(filepath.Join(dir, id+".log")); err == nil {
		matches = append(matches, filepath.Join(dir, id+".log"))
	}
	var out []segment
	for _, p := range matches {
		name := strings.TrimPrefix(filepath.Base(p), id+".")
		var s segment
		s.path = p
		switch {
		case name == "log": // written by berth 0.1 (a job still running from before an upgrade)
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			s.start, s.end, s.live = 0, info.Size(), true
		case strings.HasSuffix(name, ".log.gz"):
			a, b, ok := strings.Cut(strings.TrimSuffix(name, ".log.gz"), "-")
			start, err1 := strconv.ParseInt(a, 10, 64)
			end, err2 := strconv.ParseInt(b, 10, 64)
			if !ok || err1 != nil || err2 != nil {
				continue
			}
			s.start, s.end = start, end
		case strings.HasSuffix(name, ".log"):
			start, err := strconv.ParseInt(strings.TrimSuffix(name, ".log"), 10, 64)
			info, serr := os.Stat(p)
			if err != nil || serr != nil {
				continue
			}
			s.start, s.end, s.live = start, start+info.Size(), true
		default:
			continue
		}
		if info, err := os.Stat(p); err == nil {
			s.size = info.Size()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].start < out[b].start })
	return out
}

// Size returns a job's logical output length and bytes on disk.
func Size(dir, id string) (stream, disk int64) {
	for _, s := range segments(dir, id) {
		stream = max(stream, s.end)
		disk += s.size
	}
	return
}

// Read returns up to limit bytes of the stream from offset (negative: that many
// bytes before the end) and the offset to continue from. Dropped ranges read as
// a one-line marker.
func Read(dir, id string, offset int64, limit int) (string, int64, error) {
	segs := segments(dir, id)
	if len(segs) == 0 {
		return "", 0, nil
	}
	end := segs[len(segs)-1].end
	if offset < 0 {
		offset = max(0, end+offset)
	}
	var out bytes.Buffer
	for _, s := range segs {
		if out.Len() >= limit {
			break
		}
		if s.end <= offset {
			continue
		}
		if s.start > offset {
			fmt.Fprintf(&out, "\n[berth: %s of output dropped here to keep this log within its size limit]\n", units.FormatBytes(s.start-offset))
			offset = s.start
		}
		data, err := readSegment(s, offset-s.start, limit-out.Len())
		if err != nil {
			return out.String(), offset, err
		}
		out.Write(data)
		offset += int64(len(data))
		if offset < s.end {
			break // limit reached inside this segment
		}
	}
	return out.String(), offset, nil
}

func readSegment(s segment, skip int64, limit int) ([]byte, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // compressed or dropped between listing and reading
		}
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if !s.live {
		zr, err := gzip.NewReader(bufio.NewReader(f))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	if _, err := io.CopyN(io.Discard, r, skip); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
	buf := make([]byte, min(int64(limit), s.end-s.start-skip))
	n, err := io.ReadFull(r, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return buf[:n], err
}

// Remove deletes all of a job's log files.
func Remove(dir, id string) {
	matches, _ := filepath.Glob(filepath.Join(dir, id+".*"))
	for _, p := range matches {
		_ = os.Remove(p)
	}
}

// DirSize sums every file in the logs directory.
func DirSize(dir string) int64 {
	entries, _ := os.ReadDir(dir)
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// JobSize sums the files of one job.
func JobSize(dir, id string) int64 {
	_, disk := Size(dir, id)
	return disk
}

// Seal compresses whatever a job left uncompressed: a live segment from a
// supervisor that died, or a plain <id>.log from before segmented logs.
func Seal(dir, id string) error {
	legacy := filepath.Join(dir, id+".log")
	if _, err := os.Stat(legacy); err == nil {
		if err := os.Rename(legacy, filepath.Join(dir, id+".0.log")); err != nil {
			return err
		}
	}
	for _, s := range segments(dir, id) {
		if !s.live {
			continue
		}
		if s.end > s.start {
			if _, err := compress(s.path, filepath.Join(dir, fmt.Sprintf("%s.%d-%d.log.gz", id, s.start, s.end))); err != nil {
				return err
			}
		}
		if err := os.Remove(s.path); err != nil {
			return err
		}
	}
	return nil
}
