package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Bytes is a byte count that unmarshals from TOML size strings such as
// "50MB", "2GB" or "5GiB". It mirrors Duration: a defined scalar with
// UnmarshalText/MarshalText, so a plain TOML string becomes a typed value and
// a typo becomes an actionable error instead of a silent zero.
//
// Both unit families are accepted and they mean different things:
//
//	kB / MB / GB / TB   decimal, powers of 1000 (50MB = 50,000,000 bytes)
//	KiB / MiB / GiB/TiB binary,  powers of 1024 (50MiB = 52,428,800 bytes)
//
// Case is ignored ("50mb", "50MB" and "50Mb" are the same), a space before
// the unit is allowed ("50 MB"), the unit may be omitted for a plain byte
// count ("4096"), and a fractional value is accepted and rounded to the
// nearest byte ("1.5GB"). Negative values are rejected.
type Bytes int64

// Byte-count units, kept in one place so the parser and the formatter agree.
const (
	KB  Bytes = 1000
	MB  Bytes = 1000 * KB
	GB  Bytes = 1000 * MB
	TB  Bytes = 1000 * GB
	KiB Bytes = 1024
	MiB Bytes = 1024 * KiB
	GiB Bytes = 1024 * MiB
	TiB Bytes = 1024 * GiB
)

var byteUnits = map[string]Bytes{
	"":   1,
	"b":  1,
	"k":  KB,
	"kb": KB,
	"m":  MB,
	"mb": MB,
	"g":  GB,
	"gb": GB,
	"t":  TB,
	"tb": TB,

	"ki":  KiB,
	"kib": KiB,
	"mi":  MiB,
	"mib": MiB,
	"gi":  GiB,
	"gib": GiB,
	"ti":  TiB,
	"tib": TiB,
}

var bytesRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

// unitHelp is appended to every parse complaint so the operator does not have
// to go find the documentation.
const unitHelp = `want a size like "50MB", "2GB", "5GiB" or a plain byte count like "4096" ` +
	`(kB/MB/GB/TB are powers of 1000, KiB/MiB/GiB/TiB powers of 1024)`

// UnmarshalText implements encoding.TextUnmarshaler for BurntSushi/toml.
func (b *Bytes) UnmarshalText(text []byte) error {
	v, err := ParseBytes(string(text))
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (b Bytes) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// ParseBytes parses a size string. Exported so the CLI can accept the same
// grammar on a flag as the config file does in a key.
func ParseBytes(s string) (Bytes, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty size: %s", unitHelp)
	}
	if strings.HasPrefix(trimmed, "-") {
		return 0, fmt.Errorf("invalid size %q: a size cannot be negative", s)
	}
	m := bytesRE.FindStringSubmatch(trimmed)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q: %s", s, unitHelp)
	}
	mantissa, unit := m[1], strings.ToLower(m[2])
	// "50MBs" or "50gigs": the unit is what people get wrong, so name it.
	scale, ok := byteUnits[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size %q: unknown unit %q — %s", s, m[2], unitHelp)
	}

	if !strings.Contains(mantissa, ".") {
		n, err := strconv.ParseInt(mantissa, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q: %s", s, unitHelp)
		}
		if n != 0 && int64(scale) > math.MaxInt64/n {
			return 0, fmt.Errorf("invalid size %q: too large to be a byte count", s)
		}
		return Bytes(n) * scale, nil
	}

	f, err := strconv.ParseFloat(mantissa, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %s", s, unitHelp)
	}
	total := math.Round(f * float64(scale))
	if total > math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q: too large to be a byte count", s)
	}
	return Bytes(total), nil
}

// Bytes returns the value as a plain byte count, mirroring
// Duration.Duration().
func (b Bytes) Bytes() int64 { return int64(b) }

// String renders the value the way a human would write it: the largest unit
// that divides it exactly, decimal units tried first so the defaults round-trip
// as they are written ("50MB", "2GB") and only genuinely binary values come
// back as KiB/MiB/GiB. It falls back to a bare byte count.
func (b Bytes) String() string {
	if b == 0 {
		return "0"
	}
	if b < 0 {
		return strconv.FormatInt(int64(b), 10)
	}
	for _, u := range []struct {
		size Bytes
		name string
	}{
		{TB, "TB"}, {GB, "GB"}, {MB, "MB"}, {KB, "kB"},
		{TiB, "TiB"}, {GiB, "GiB"}, {MiB, "MiB"}, {KiB, "KiB"},
	} {
		if b%u.size == 0 {
			return strconv.FormatInt(int64(b/u.size), 10) + u.name
		}
	}
	return strconv.FormatInt(int64(b), 10)
}
