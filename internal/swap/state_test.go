package swap

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	s, err := Load[Session](filepath.Join(dir, "none.json"))
	if err != nil || s != (Session{}) {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	p := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(p, []byte("{nope"), 0o600)
	if _, err := Load[Session](p); err == nil {
		t.Fatal("corrupt file must fail")
	}
}

func TestUpdateWritesPrivateFileAtomically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "dir")
	p := filepath.Join(dir, "request.json")
	if err := Update(p, func(q *Request) { q.Pin = "pi"; q.Sync = []string{"*"} }); err != nil {
		t.Fatal(err)
	}
	q, err := Load[Request](p)
	if err != nil || q.Pin != "pi" || len(q.Sync) != 1 {
		t.Fatalf("round trip: %+v %v", q, err)
	}
	if m := mode(t, p); m != 0o600 {
		t.Errorf("file mode %o", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode %o", m)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "request.json" && e.Name() != "request.json.lock" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestUpdateConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Update(p, func(s *Session) { s.Turns++ }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s, _ := Load[Session](p); s.Turns != 20 {
		t.Fatalf("lost updates: %d of 20", s.Turns)
	}
}

// A symlink planted at the old fixed temp name (path+".tmp") is not followed.
func TestUpdateIgnoresSymlinkedTmp(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	p := filepath.Join(dir, "session.json")
	if err := os.Symlink(victim, p+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := Update(p, func(s *Session) { s.NativeID = "x" }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("symlink target overwritten: %q", b)
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("state file must be a regular file: %v", err)
	}
	if s, _ := Load[Session](p); s.NativeID != "x" {
		t.Fatalf("update lost: %+v", s)
	}
}

// A symlinked lock file is refused, so its target is never created or truncated.
func TestUpdateRefusesSymlinkedLock(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	p := filepath.Join(dir, "session.json")
	for _, target := range []string{filepath.Join(outside, "existing"), filepath.Join(outside, "absent")} {
		if strings.HasSuffix(target, "existing") {
			_ = os.WriteFile(target, []byte("keep"), 0o600)
		}
		_ = os.Remove(p + ".lock")
		if err := os.Symlink(target, p+".lock"); err != nil {
			t.Fatal(err)
		}
		if err := Update(p, func(s *Session) { s.NativeID = "x" }); err == nil {
			t.Errorf("%s: Update followed a symlinked lock", filepath.Base(target))
		}
		b, err := os.ReadFile(target)
		switch filepath.Base(target) {
		case "existing":
			if string(b) != "keep" {
				t.Errorf("lock target changed: %q", b)
			}
		case "absent":
			if err == nil {
				t.Error("lock target created through the symlink")
			}
		}
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("state written without the lock")
	}
}
