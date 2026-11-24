// Package units parses and formats byte sizes and durations for humans and agents.
package units

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
	TiB int64 = 1 << 40
)

// ParseBytes accepts "512M", "4G", "1.5GiB", "2048" (bytes), "4g", "100%"-free forms are not supported.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	t = strings.TrimSuffix(strings.TrimSuffix(t, "ib"), "b")
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "k"):
		mult = KiB
	case strings.HasSuffix(t, "m"):
		mult = MiB
	case strings.HasSuffix(t, "g"):
		mult = GiB
	case strings.HasSuffix(t, "t"):
		mult = TiB
	}
	if mult != 1 {
		t = t[:len(t)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("invalid size %q (examples: 512M, 4G, 1.5G)", s)
	}
	return int64(v * float64(mult)), nil
}

// FormatBytes renders a size compactly: 4G, 1.5G, 512M.
func FormatBytes(b int64) string {
	neg := b < 0
	if neg {
		b = -b
	}
	var out string
	switch {
	case b >= TiB:
		out = trim(float64(b)/float64(TiB)) + "T"
	case b >= GiB:
		out = trim(float64(b)/float64(GiB)) + "G"
	case b >= MiB:
		out = trim(float64(b)/float64(MiB)) + "M"
	case b >= KiB:
		out = trim(float64(b)/float64(KiB)) + "K"
	default:
		out = strconv.FormatInt(b, 10) + "B"
	}
	if neg {
		return "-" + out
	}
	return out
}

func trim(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(v, 'f', 1, 64), "0"), ".")
}

// ParseDuration accepts Go durations plus bare seconds ("90") and days ("2d").
func ParseDuration(s string) (time.Duration, error) {
	t := strings.TrimSpace(s)
	if t == "" || t == "0" {
		return 0, nil
	}
	if v, err := strconv.ParseFloat(t, 64); err == nil {
		return time.Duration(v * float64(time.Second)), nil
	}
	if strings.HasSuffix(t, "d") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(t, "d"), 64)
		if err == nil {
			return time.Duration(v * 24 * float64(time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(t)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q (examples: 90s, 10m, 2h)", s)
	}
	return d, nil
}

// FormatDuration renders 3s, 4m, 2h5m, 3d.
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// Bytes is a size that reads from TOML/JSON as either "4G" or a number of bytes.
type Bytes int64

func (b *Bytes) UnmarshalText(text []byte) error {
	v, err := ParseBytes(string(text))
	*b = Bytes(v)
	return err
}

func (b Bytes) MarshalText() ([]byte, error) { return []byte(FormatBytes(int64(b))), nil }

func (b *Bytes) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		*b = Bytes(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	return b.UnmarshalText([]byte(s))
}

func (b Bytes) MarshalJSON() ([]byte, error) { return json.Marshal(int64(b)) }

// Duration reads from TOML as "10m" and from JSON as seconds or a string.
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := ParseDuration(string(text))
	*d = Duration(v)
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var f float64
	if err := json.Unmarshal(data, &f); err == nil {
		*d = Duration(f * float64(time.Second))
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	return d.UnmarshalText([]byte(s))
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).Seconds()) }

// FormatBytesDown renders a size rounded down, for limits a request must stay within.
func FormatBytesDown(b int64) string {
	switch {
	case b >= GiB:
		return trim(math.Floor(float64(b)/float64(GiB)*10)/10) + "G"
	case b >= MiB:
		return strconv.FormatInt(b/MiB, 10) + "M"
	}
	return FormatBytes(b)
}
