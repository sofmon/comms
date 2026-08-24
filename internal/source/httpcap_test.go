package source

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubTransport returns a fixed body for every request.
type stubTransport struct{ body []byte }

func (t *stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     make(http.Header),
	}, nil
}

func roundTrip(t *testing.T, rt http.RoundTripper) ([]byte, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// TestCapResponseBodyStopsAtTheCap: the wrapper must FAIL rather than hand up
// a short read. A truncated body presented as a clean EOF is the dangerous
// outcome — a JSON decoder would report "unexpected end of input", or worse,
// succeed on a partial object.
func TestCapResponseBodyStopsAtTheCap(t *testing.T) {
	rt := CapResponseBody(&stubTransport{body: bytes.Repeat([]byte("x"), 1000)}, 100)
	got, err := roundTrip(t, rt)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v (read %d bytes), want ErrResponseTooLarge", err, len(got))
	}
}

// TestCapResponseBodyPassesUnderTheCap, including a body of exactly the cap.
func TestCapResponseBodyPassesUnderTheCap(t *testing.T) {
	for _, n := range []int{0, 1, 99, 100} {
		body := bytes.Repeat([]byte("x"), n)
		rt := CapResponseBody(&stubTransport{body: body}, 100)
		got, err := roundTrip(t, rt)
		if err != nil {
			t.Fatalf("%d bytes under a 100-byte cap: %v", n, err)
		}
		if len(got) != n {
			t.Errorf("read %d bytes, want %d", len(got), n)
		}
	}
}

// oneByteReader delivers a single byte per Read, like a real socket under
// load. The cap must hold across chunk boundaries, not just when the whole
// body arrives in one read.
type oneByteReader struct {
	b []byte
	i int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.b[r.i]
	r.i++
	return 1, nil
}

func (r *oneByteReader) Close() error { return nil }

func TestCapResponseBodyAcrossChunks(t *testing.T) {
	for _, c := range []struct {
		n       int
		wantErr bool
	}{{9, false}, {10, false}, {11, true}} {
		rc := &oneByteReader{b: bytes.Repeat([]byte("x"), c.n)}
		r := &capReader{rc: rc, left: 10, max: 10}
		got, err := io.ReadAll(r)
		switch {
		case c.wantErr && !errors.Is(err, ErrResponseTooLarge):
			t.Errorf("%d bytes over a 10-byte cap: err = %v, want ErrResponseTooLarge", c.n, err)
		case !c.wantErr && err != nil:
			t.Errorf("%d bytes under a 10-byte cap: %v", c.n, err)
		case !c.wantErr && len(got) != c.n:
			t.Errorf("read %d bytes, want %d", len(got), c.n)
		}
	}
}

// TestCapResponseBodyDisabled: a non-positive cap installs no wrapper at all,
// so an operator who set max_message_bytes = 0 pays nothing for the feature.
func TestCapResponseBodyDisabled(t *testing.T) {
	base := &stubTransport{body: []byte("hello")}
	if got := CapResponseBody(base, 0); got != http.RoundTripper(base) {
		t.Errorf("cap 0 returned %T, want the base transport unchanged", got)
	}
	if got := CapResponseBody(base, -1); got != http.RoundTripper(base) {
		t.Errorf("negative cap returned %T, want the base transport unchanged", got)
	}
}

// TestCapResponseBodyDefaultsBase: a nil base is the zero value of
// http.Client.Transport, which means http.DefaultTransport. Wrapping nil must
// not produce a transport that panics on first use.
func TestCapResponseBodyDefaultsBase(t *testing.T) {
	rt := CapResponseBody(nil, 100)
	ct, ok := rt.(*capTransport)
	if !ok {
		t.Fatalf("got %T, want *capTransport", rt)
	}
	if ct.next != http.DefaultTransport {
		t.Errorf("base = %v, want http.DefaultTransport", ct.next)
	}
}

func TestReadAllCapped(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		max     int64
		wantErr bool
	}{
		{"under", "hello", 100, false},
		{"exactly at the cap", "hello", 5, false},
		{"one over", "hello!", 5, true},
		{"cap disabled", "hello", 0, false},
		{"empty", "", 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ReadAllCapped(strings.NewReader(c.body), c.max)
			if c.wantErr {
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("err = %v, want ErrResponseTooLarge", err)
				}
				if got != nil {
					t.Errorf("returned %q alongside the error; an over-cap read must yield nothing", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if string(got) != c.body {
				t.Errorf("got %q, want %q", got, c.body)
			}
		})
	}
}

// TestFreeSpaceContract: whatever the platform answers, the contract callers
// rely on holds — a usable number, or "unknown" (<= 0), never a number that
// would silently disable or trigger the floor by accident.
func TestFreeSpaceContract(t *testing.T) {
	n, err := FreeSpace(t.TempDir())
	if err != nil {
		if n != 0 {
			t.Errorf("FreeSpace returned %d with error %v; a failed probe must answer 0 (unknown)", n, err)
		}
		return
	}
	if n <= 0 {
		t.Errorf("FreeSpace = %d with no error; a temp dir's volume should report positive free space", n)
	}
}
