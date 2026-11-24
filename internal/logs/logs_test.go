package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func line(i int) string { return fmt.Sprintf("line %06d %s\n", i, strings.Repeat("x", 40)) }

func readAll(t *testing.T, dir, id string) string {
	t.Helper()
	var b strings.Builder
	offset := int64(0)
	for i := 0; i < 1000; i++ {
		data, next, err := Read(dir, id, offset, 64<<10)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(data)
		if next == offset {
			break
		}
		offset = next
	}
	return b.String()
}

func TestRoundTripCompressedSegments(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, "j", 4096, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := 0; i < 2000; i++ {
		want.WriteString(line(i))
		fmt.Fprint(w, line(i))
	}
	live := readAll(t, dir, "j") // readable while running
	if live != want.String() {
		t.Fatalf("live read differs: %d vs %d bytes", len(live), want.Len())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, dir, "j"); got != want.String() {
		t.Fatalf("finished read differs: %d vs %d bytes", len(got), want.Len())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "j.*"))
	for _, f := range files {
		if !strings.HasSuffix(f, ".log.gz") {
			t.Fatalf("finished logs must be fully compressed, found %s", f)
		}
	}
	stream, disk := Size(dir, "j")
	if stream != int64(want.Len()) || disk*5 > stream {
		t.Fatalf("stream %d, disk %d: repetitive output should compress at least 5x", stream, disk)
	}
}

func TestKeepsHeadAndTailWithinBudget(t *testing.T) {
	dir := t.TempDir()
	w, _ := Create(dir, "j", 4096, 3000) // about three compressed segments
	for i := 0; i < 20000; i++ {
		fmt.Fprint(w, line(i))
	}
	w.Close()
	if _, disk := Size(dir, "j"); disk > 3000+4096 {
		t.Fatalf("disk %d exceeds the budget", disk)
	}
	got := readAll(t, dir, "j")
	if !strings.HasPrefix(got, line(0)) || !strings.HasSuffix(got, line(19999)) || !strings.Contains(got, "output dropped here") {
		t.Fatalf("want head, marker, tail; got %d bytes starting %q", len(got), got[:60])
	}
}

func TestTailRead(t *testing.T) {
	dir := t.TempDir()
	w, _ := Create(dir, "j", 4096, 1<<20)
	for i := 0; i < 500; i++ {
		fmt.Fprint(w, line(i))
	}
	w.Close()
	data, _, err := Read(dir, "j", -int64(len(line(0))*2), 1<<20)
	if err != nil || data != line(498)+line(499) {
		t.Fatalf("tail: %q %v", data, err)
	}
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	r, _ := OpenRotating(path, 1000, 2)
	for i := 0; i < 200; i++ {
		fmt.Fprint(r, line(i))
	}
	for _, p := range []string{path, path + ".1.gz", path + ".2.gz"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	if _, err := os.Stat(path + ".3.gz"); err == nil {
		t.Fatal("kept more than 2 rotations")
	}
}

func TestSealLegacyAndOrphanedLogs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "old.log"), []byte(line(1)+line(2)), 0o600)
	if err := Seal(dir, "old"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, dir, "old"); got != line(1)+line(2) {
		t.Fatalf("legacy log lost: %q", got)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "old.*"))
	if len(files) != 1 || !strings.HasSuffix(files[0], ".log.gz") {
		t.Fatalf("want one compressed segment, got %v", files)
	}
}

func TestReadsLegacyLiveLog(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "old.log"), []byte(line(7)), 0o600)
	if got := readAll(t, dir, "old"); got != line(7) {
		t.Fatalf("a job still writing a 0.1 log must stay readable: %q", got)
	}
}
