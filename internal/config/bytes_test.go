package config

import (
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Bytes
	}{
		{"0", 0},
		{"1", 1},
		{"4096", 4096},
		{"512B", 512},
		{"50MB", 50_000_000},
		{"50mb", 50_000_000},
		{"50 MB", 50_000_000},
		{"  50MB  ", 50_000_000},
		{"2GB", 2_000_000_000},
		{"5GB", 5_000_000_000},
		{"100MB", 100_000_000},
		{"150MB", 150_000_000},
		{"1kB", 1000},
		{"1k", 1000},
		{"1TB", 1_000_000_000_000},
		{"5GiB", 5 * 1024 * 1024 * 1024},
		{"50MiB", 52_428_800},
		{"1KiB", 1024},
		{"1kib", 1024},
		{"1TiB", 1 << 40},
		{"1.5GB", 1_500_000_000},
		{"0.5MiB", 524_288},
		{"2.25kB", 2250},
	} {
		got, err := ParseBytes(tc.in)
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseBytesErrors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "empty size"},
		{"   ", "empty size"},
		{"-1", "cannot be negative"},
		{"-5MB", "cannot be negative"},
		{"MB", "invalid size"},
		{"50 mega", "unknown unit"},
		{"50MBs", "unknown unit"},
		{"5 0MB", "invalid size"},
		{"1e9", "invalid size"},
		{"50%", "invalid size"},
		{"99999999999999999999TB", "invalid size"},
		{"9223372036854775807TB", "too large"},
	} {
		_, err := ParseBytes(tc.in)
		if err == nil {
			t.Errorf("ParseBytes(%q) should have failed", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseBytes(%q) error %v does not mention %q", tc.in, err, tc.want)
		}
	}
}

func TestBytesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		v    Bytes
		want string
	}{
		{0, "0"},
		{50 * MB, "50MB"},
		{100 * MB, "100MB"},
		{150 * MB, "150MB"},
		{2 * GB, "2GB"},
		{5 * GB, "5GB"},
		{50 * MiB, "50MiB"},
		{1 * GiB, "1GiB"},
		{4096, "4KiB"},
		{1, "1"},
		{999, "999"},
	} {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("Bytes(%d).String() = %q, want %q", tc.v, got, tc.want)
		}
		back, err := ParseBytes(tc.v.String())
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", tc.v.String(), err)
			continue
		}
		if back != tc.v {
			t.Errorf("round trip of %d via %q gave %d", tc.v, tc.v.String(), back)
		}
	}
}

func TestBytesUnmarshalText(t *testing.T) {
	var b Bytes
	if err := b.UnmarshalText([]byte("50MB")); err != nil {
		t.Fatal(err)
	}
	if b.Bytes() != 50_000_000 {
		t.Errorf("Bytes() = %d", b.Bytes())
	}
	if err := b.UnmarshalText([]byte("nope")); err == nil {
		t.Error("UnmarshalText must reject garbage")
	}
	if b.Bytes() != 50_000_000 {
		t.Error("a failed UnmarshalText must not clobber the previous value")
	}
	text, err := b.MarshalText()
	if err != nil || string(text) != "50MB" {
		t.Errorf("MarshalText = %q, %v", text, err)
	}
}
