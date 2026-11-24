package units

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	for in, want := range map[string]int64{"4G": 4 * GiB, "1.5g": GiB * 3 / 2, "512M": 512 * MiB, "512mib": 512 * MiB, "2048": 2048, "1T": TiB} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "-1G", "4X"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) should fail", bad)
		}
	}
	if s := FormatBytes(3 * GiB / 2); s != "1.5G" {
		t.Errorf("FormatBytes = %s", s)
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"90": 90 * time.Second, "10m": 10 * time.Minute, "2d": 48 * time.Hour, "0": 0} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if FormatDuration(125*time.Minute) != "2h5m" {
		t.Error(FormatDuration(125 * time.Minute))
	}
}
func TestFormatDown(t *testing.T) {
	if s := FormatBytesDown(GiB * 7 / 4); s != "1.7G" {
		t.Fatal(s)
	}
}
