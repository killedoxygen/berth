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
		data, next, err := Plain(dir).Read(id, offset, 64<<10)
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
	stream, disk := Plain(dir).Size("j")
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
	if _, disk := Plain(dir).Size("j"); disk > 3000+4096 {
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
	data, _, err := Plain(dir).Read("j", -int64(len(line(0))*2), 1<<20)
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
	if err := Plain(dir).Seal("old"); err != nil {
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

// A root daemon reads logs in directories their users control. Nothing a user
// plants there may make it read or write other files.
func TestPlantedSymlinksAreNeverFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	os.WriteFile(secret, []byte("TOP SECRET\n"), 0o600)
	os.Symlink(secret, filepath.Join(dir, "j.0.log"))
	os.Symlink(secret, filepath.Join(dir, "j.11-22.log.gz"))
	d := Dir{Path: dir, Owner: os.Getuid()}
	if data, _, _ := d.Read("j", 0, 1<<20); strings.Contains(data, "SECRET") {
		t.Fatal("read through a symlink")
	}
	if err := d.Seal("j"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(secret); string(data) != "TOP SECRET\n" {
		t.Fatal("the symlink target was modified")
	}
	// A planted destination name must not redirect the compressed write.
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("keep\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "k.0.log"), []byte("hello"), 0o600)
	os.Symlink(victim, filepath.Join(dir, "k.0-5.log.gz"))
	if err := d.Seal("k"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep\n" {
		t.Fatal("seal wrote through a planted symlink")
	}
	if data, _, _ := d.Read("k", 0, 100); data != "hello" {
		t.Fatalf("sealed log: %q", data)
	}
}

func TestForeignOwnedFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "j.0.log"), []byte("mine"), 0o600)
	if data, _, _ := (Dir{Path: dir, Owner: os.Getuid() + 1}).Read("j", 0, 100); data != "" {
		t.Fatal("a file owned by someone else must not be served as this user's log")
	}
}
