package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

func TestDefaultsAreValidOnAnyHost(t *testing.T) {
	for _, h := range []probe.Host{
		{MemTotal: 64 * units.GiB, CPUs: 24, HomeDiskTotal: units.TiB},
		{MemTotal: 2 * units.GiB, CPUs: 1},
		{MemTotal: 64 * units.GiB, CPUs: 8, ParentLimit: 512 * units.MiB}, // inside a small cgroup
		{},
	} {
		c := Default(h)
		if err := c.Validate(); err != nil {
			t.Errorf("host %+v: %v", h, err)
		}
	}
}

func TestFileOverridesOnlyWhatItNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[pool]\nmemory = \"48G\"\n"), 0o600)
	h := probe.Host{MemTotal: 64 * units.GiB, CPUs: 16}
	c, err := Load(path, h)
	if err != nil || c.Pool.Memory != units.Bytes(48*units.GiB) || c.Pool.CPUs != Default(h).Pool.CPUs {
		t.Fatalf("%v %+v", err, c.Pool)
	}
	os.WriteFile(path, []byte("[pool]\nmemroy = \"48G\"\n"), 0o600)
	if _, err := Load(path, h); err == nil {
		t.Fatal("a misspelled setting must be an error, not silently ignored")
	}
}
