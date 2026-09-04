package lock

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireContention(t *testing.T) {
	dir := t.TempDir()

	release1, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// flock is per-open-file-description, so a second open in the same
	// process contends exactly like a second process would.
	if _, err := Acquire(dir); err == nil {
		t.Fatal("second Acquire should fail while the lock is held")
	} else if !strings.Contains(err.Error(), "another comms operation is running") {
		t.Fatalf("contention error should say another instance is running, got: %v", err)
	}

	release1()

	release2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	release2()
	release2() // idempotent — second call must not panic or double-close
}

func TestLockFileDetails(t *testing.T) {
	dir := t.TempDir()
	release, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	path := filepath.Join(dir, "lock")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("lock file mode = %04o, want no group/world access", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("lock file records pid %q, want %q", got, want)
	}
}

func TestAcquireNamedIsIndependent(t *testing.T) {
	dir := t.TempDir()
	releaseArchive, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseArchive()
	releaseSend, err := AcquireNamed(dir, "send.lock")
	if err != nil {
		t.Fatalf("named lock should coexist with archive lock: %v", err)
	}
	defer releaseSend()
	if _, err := AcquireNamed(dir, "send.lock"); err == nil {
		t.Fatal("second send lock should contend")
	}
	if _, err := AcquireNamed(dir, "../bad"); err == nil {
		t.Fatal("unsafe lock name accepted")
	}
}

func TestAcquireMissingDir(t *testing.T) {
	if _, err := Acquire(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("Acquire in a missing directory should fail")
	}
}
