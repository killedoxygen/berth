package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

func cfg() *config.Config {
	c := config.Default(probe.Host{MemTotal: 64 * units.GiB, CPUs: 16, HomeDiskTotal: 500 * units.GiB})
	return &c
}

func TestContainerRunGetsLimitsAndIsChargedByThem(t *testing.T) {
	c := cfg()
	d := Route(c, "docker", []string{"run", "--rm", "-v", "/a:/b", "img", "cmd", "-d"}, "/", false)
	if !d.Queue || strings.Join(d.Argv[:5], " ") != "run --memory 4G --cpus 2" {
		t.Fatalf("got %+v", d)
	}
	if d.Memory != 4*units.GiB || d.Unit != 512*units.MiB {
		t.Fatalf("charged %d, unit cap %d", d.Memory, d.Unit)
	}
	d = Route(c, "podman", []string{"run", "-m", "512m", "--cpus=1", "img"}, "/", false)
	if strings.Join(d.Argv, " ") != "run -m 512m --cpus=1 img" || d.Memory != 512*units.MiB || d.CPUs != 1 {
		t.Fatalf("explicit limits must be kept and charged: %+v", d)
	}
}

func TestContainerLightOrDetachedNotQueued(t *testing.T) {
	c := cfg()
	for _, args := range [][]string{{"ps"}, {"exec", "x", "ls"}, {"pull", "img"}, {"compose", "up"}, {"--version"}} {
		if Route(c, "docker", args, "/", false).Queue {
			t.Fatalf("%v should run directly", args)
		}
	}
	d := Route(c, "docker", []string{"run", "-dit", "img"}, "/", false)
	if d.Queue || !strings.Contains(strings.Join(d.Argv, " "), "--memory") {
		t.Fatalf("detached runs are not queued but still get limits: %+v", d)
	}
	for _, args := range [][]string{{"build", "."}, {"buildx", "build", "."}, {"--context", "x", "build", "."}, {"compose", "build"}} {
		if !Route(c, "docker", args, "/", false).Queue {
			t.Fatalf("%v should be queued", args)
		}
	}
}

func TestBuildTools(t *testing.T) {
	c := cfg()
	cases := map[string]bool{"cargo build": true, "cargo t": true, "cargo metadata": false, "make": true, "make -n": false,
		"ninja -t targets": false, "cmake --build b": true, "cmake -S . -B b": false, "go test ./...": true, "go env": false}
	for line, want := range cases {
		f := strings.Fields(line)
		if got := Route(c, f[0], f[1:], "/", false).Queue; got != want {
			t.Errorf("%s: queue=%v, want %v", line, got, want)
		}
	}
}

func TestConfigRule(t *testing.T) {
	c := cfg()
	c.Shims.Rules = []config.ShimRule{{Tool: "npm", Subcommands: []string{"ci", "test"}, Memory: units.Bytes(3 * units.GiB)}}
	if d := Route(c, "npm", []string{"ci"}, "/", false); !d.Queue || d.Memory != 3*units.GiB {
		t.Fatalf("rule not applied: %+v", d)
	}
	if Route(c, "npm", []string{"view", "x"}, "/", false).Queue {
		t.Fatal("other subcommands run directly")
	}
	c.Shims.Rules = []config.ShimRule{{Tool: "gdb", FlagsAny: []string{"-batch", "--batch"}}}
	if !Route(c, "gdb", []string{"-batch", "-ex", "bt", "core"}, "/", false).Queue || Route(c, "gdb", []string{"./prog"}, "/", false).Queue {
		t.Fatal("flag rules: batch gdb is queued, interactive gdb is not")
	}
}

func repo(t *testing.T, config string, heavy bool) string {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644)
	if heavy {
		os.WriteFile(filepath.Join(gitDir, "berth-heavy"), nil, 0o644)
	}
	sub := filepath.Join(dir, "src")
	os.MkdirAll(sub, 0o755)
	return sub
}

func TestGitReadOnlyNeverQueued(t *testing.T) {
	c := cfg()
	dir := repo(t, "", true)
	for _, args := range [][]string{{"log"}, {"status"}, {"diff"}, {"branch"}, {"for-each-ref"}, {"hash-object", "x"}, {"show"}, {"commit", "-m", "x"}} {
		if Route(c, "git", args, dir, false).Queue {
			t.Fatalf("git %v must run directly", args)
		}
	}
	d := Route(c, "git", []string{"-C", ".", "fetch"}, dir, false)
	if !d.Queue || !strings.HasPrefix(d.Group, "git:") || d.Timeout != Route(c, "git", []string{"gc"}, dir, false).Timeout {
		t.Fatalf("fetch in a large repo is queued per repository with the long timeout: %+v", d)
	}
	if Route(c, "git", []string{"fetch"}, dir, true).Queue {
		t.Fatal("interactive git runs directly")
	}
}

func TestGitPartialCloneOnlyNetworkWork(t *testing.T) {
	c := cfg()
	dir := repo(t, "[remote \"origin\"]\n\tpromisor = true\n", false)
	if !Route(c, "git", []string{"fetch"}, dir, false).Queue {
		t.Fatal("network work in a partial clone is heavy")
	}
	if Route(c, "git", []string{"checkout", "x"}, dir, false).Queue {
		t.Fatal("a partial clone is not large in itself")
	}
}

func TestRealSkipsToolsAlreadyInTheChain(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, d := range []string{a, b} {
		os.WriteFile(filepath.Join(d, "tool"), []byte("#!/bin/sh\n"), 0o755)
	}
	t.Setenv("PATH", a+":"+b)
	t.Setenv(seenVar, "")
	if got := Real("tool", "/nonexistent/berth"); got != filepath.Join(a, "tool") {
		t.Fatalf("first candidate: %s", got)
	}
	t.Setenv(seenVar, filepath.Join(a, "tool"))
	if got := Real("tool", "/nonexistent/berth"); got != filepath.Join(b, "tool") {
		t.Fatalf("a wrapper that called back into berth must be skipped: %s", got)
	}
}
