package swap

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// envGet looks key up in env (KEY=VALUE pairs); later entries win.
func envGet(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// storeRoot is where a harness kind keeps its sessions ("" = not a file store).
func storeRoot(kind string, rt Runtime) string {
	or := func(key string, def ...string) string {
		if v := envGet(rt.Env, key); v != "" {
			return v
		}
		return filepath.Join(append([]string{rt.Home}, def...)...)
	}
	switch kind {
	case "claude":
		return or("CLAUDE_CONFIG_DIR", ".claude")
	case "pi":
		return or("PI_CODING_AGENT_SESSION_DIR", ".pi", "agent", "sessions")
	case "codex":
		return or("CODEX_HOME", ".codex")
	}
	return ""
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeSlug is claude's project dir name for cwd.
func claudeSlug(cwd string) string { return slugUnsafe.ReplaceAllString(cwd, "-") }

// piSlug is pi's session dir name for cwd.
func piSlug(cwd string) string {
	return "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.TrimPrefix(cwd, "/")) + "--"
}

// sessionPath locates the JSONL file of a native session in its kind's store:
// a regular, single-link file under the store root. claude and pi files are
// looked up in the cwd's project dir first (claude then any project dir: long
// cwds get a hashed slug). A missing file wraps os.ErrNotExist.
func sessionPath(kind, native string, rt Runtime) (string, error) {
	if !safeID(native) {
		return "", fmt.Errorf("bad %s session id %q", kind, native)
	}
	root := storeRoot(kind, rt)
	var path string
	switch kind {
	case "claude":
		path = filepath.Join(root, "projects", claudeSlug(rt.Cwd), native+".jsonl")
		if _, err := os.Lstat(path); err != nil {
			path = ""
			if m, _ := filepath.Glob(filepath.Join(root, "projects", "*", native+".jsonl")); len(m) > 0 { // native is safeID: no glob syntax
				path = m[len(m)-1]
			}
		}
	case "pi":
		if path = findIn(filepath.Join(root, piSlug(rt.Cwd)), "", "_"+native+".jsonl"); path == "" {
			path = findIn(root, "", "_"+native+".jsonl")
		}
	case "codex":
		for _, y := range dirs(filepath.Join(root, "sessions")) {
			for _, m := range dirs(y) {
				for _, d := range dirs(m) {
					if p := findIn(d, "rollout-", "-"+native+".jsonl"); p != "" {
						path = p
					}
				}
			}
		}
	default:
		return "", errNoMirror
	}
	if path == "" {
		return "", fmt.Errorf("%s session %s: %w", kind, native, os.ErrNotExist)
	}
	if _, err := regular(path); err != nil {
		return "", err
	}
	return path, within(root, path)
}

// dirs lists the subdirectories of dir, sorted.
func dirs(dir string) []string {
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// findIn is the last (sorted) entry of dir named prefix…suffix ("" = none).
func findIn(dir, prefix, suffix string) string {
	es, _ := os.ReadDir(dir)
	p := ""
	for _, e := range es {
		if n := e.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			p = filepath.Join(dir, n)
		}
	}
	return p
}

// regular lstats path: it must be a regular, non-symlink file with one link.
func regular(path string) (*syscall.Stat_t, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Nlink != 1 {
		return nil, fmt.Errorf("%s: not a regular single-link file", path)
	}
	return st, nil
}

// within checks that path's directory, symlinks resolved, is under root.
func within(root, path string) error {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	d, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(r, d); err != nil || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return fmt.Errorf("%s is outside %s", path, root)
	}
	return nil
}

// createExcl creates path (never existing, never a symlink) under root with
// data, mode 0600, in one write plus fsync, and returns its identity.
func createExcl(root, path string, data []byte) (dev, ino uint64, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 0, 0, fmt.Errorf("bad path %q", path)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return 0, 0, err
	}
	d := filepath.Dir(path) // check the nearest existing ancestor before creating dirs
	for _, err := os.Lstat(d); err != nil && d != filepath.Dir(d); _, err = os.Lstat(d) {
		d = filepath.Dir(d)
	}
	if err := within(root, filepath.Join(d, "x")); err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, 0, err
	}
	if err := within(root, path); err != nil {
		return 0, 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	if err := writeSync(f, data); err != nil {
		_ = os.Remove(path)
		return 0, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	st := fi.Sys().(*syscall.Stat_t)
	return uint64(st.Dev), st.Ino, nil
}

func writeSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// cursorOffset parses a JSONL mirror's byte-offset cursor ("" = 0).
func cursorOffset(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	return strconv.ParseInt(c, 10, 64)
}

// ownedStat checks the open or lstat'ed file against the mirror's identity:
// the same dev/inode, regular, one link, and exactly as long as the cursor.
func ownedStat(st *syscall.Stat_t, m *Mirror) error {
	off, err := cursorOffset(m.Cursor)
	if err != nil {
		return err
	}
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1:
		return fmt.Errorf("%s: not a regular single-link file", m.Path)
	case uint64(st.Dev) != m.Dev || st.Ino != m.Ino:
		return fmt.Errorf("%s: file was replaced", m.Path)
	case st.Size != off:
		return fmt.Errorf("%s: size %d is not the cursor %d", m.Path, st.Size, off)
	}
	return nil
}

// checkOwned is the identity check run before an Append. root "" (no file
// store, opencode) checks nothing.
func checkOwned(root string, m *Mirror) error {
	if root == "" {
		return nil
	}
	if m.Path == "" || !filepath.IsAbs(m.Path) || filepath.Clean(m.Path) != m.Path || !strings.Contains(filepath.Base(m.Path), m.Native) || !safeID(m.Native) {
		return fmt.Errorf("mirror path %q does not match session %q", m.Path, m.Native)
	}
	if err := within(root, m.Path); err != nil {
		return err
	}
	st, err := regular(m.Path)
	if err != nil {
		return err
	}
	return ownedStat(st, m)
}

// appendOwned appends data to the mirror's file in one write plus fsync,
// after checking on the open fd that it is still the file the mirror owns.
// m.Cursor becomes the new file size.
func appendOwned(m *Mirror, data []byte) error {
	f, err := os.OpenFile(m.Path, os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := ownedStat(fi.Sys().(*syscall.Stat_t), m); err != nil {
		return err
	}
	if err := writeSync(f, data); err != nil {
		return err
	}
	m.Cursor = strconv.FormatInt(fi.Size()+int64(len(data)), 10)
	return nil
}

// bornAfter reports whether a JSONL session's first top-level timestamp
// (within its first 4 MiB) is not before t: the session was created after t.
func bornAfter(path string, t time.Time) bool {
	born := false
	scanHead(path, func(line []byte) bool {
		var l struct{ Timestamp string }
		if json.Unmarshal(line, &l) != nil || l.Timestamp == "" {
			return false
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		born = err == nil && !ts.Before(t)
		return true
	})
	return born
}

// scanHead calls fn for each line of path's first 4 MiB until fn returns true.
// Lines of any length work (bufio.Reader, not Scanner).
func scanHead(path string, fn func([]byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	br := bufio.NewReader(io.LimitReader(f, 4<<20))
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && fn(line) || err != nil {
			return
		}
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func fmtUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func uuid4() string {
	b := randBytes(16)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmtUUID(b)
}

func uuid7() string {
	b := randBytes(16)
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6], b[8] = b[6]&0x0f|0x70, b[8]&0x3f|0x80
	return fmtUUID(b)
}

func hex8() string { return hex.EncodeToString(randBytes(4)) }

var ocID struct {
	sync.Mutex
	last    uint64
	counter uint64
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// opencodeID mimics opencode 1.19 ids: prefix_ + 12 hex of (ms*0x1000 +
// counter) mod 2^48 + 14 base62 random chars. msg_/prt_ ascend within the
// process; ses_ uses the bitwise NOT, so sessions descend.
func opencodeID(prefix string, t time.Time) string {
	ocID.Lock()
	ms := uint64(t.UnixMilli())
	if ms <= ocID.last {
		ms = ocID.last // never go back in time
		ocID.counter++
	} else {
		ocID.last, ocID.counter = ms, 0
	}
	v := ms*0x1000 + ocID.counter
	ocID.Unlock()
	if prefix == "ses" { // session ids descend: newer sorts first
		v = ^v
	}
	var b strings.Builder
	b.WriteString(prefix + "_")
	fmt.Fprintf(&b, "%012x", v&0xffffffffffff)
	for i := 0; i < 14; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(62))
		b.WriteByte(base62[n.Int64()])
	}
	return b.String()
}

var errNoMirror = errors.New("no mirror support")

// sessionCwd is the cwd a JSONL session records: the first top-level `cwd`
// (claude entries, the pi header) or codex's session_meta payload.cwd, found
// within the file's first 4 MiB. "" if none yet.
func sessionCwd(path string) string {
	cwd := ""
	scanHead(path, func(line []byte) bool {
		var l struct {
			Type, Cwd string
			Payload   struct{ Cwd string }
		}
		if json.Unmarshal(line, &l) != nil {
			return false
		}
		if l.Type == "session_meta" {
			cwd = l.Payload.Cwd
			return true
		}
		cwd = l.Cwd
		return cwd != ""
	})
	return cwd
}

var versions sync.Map // bin → version

var versionTok = regexp.MustCompile(`\d+\.\d+(\.\d+)?[0-9A-Za-z.+-]*`)

// binVersion is the first version-looking token of `bin --version` (5 s
// timeout), or "0.0.0"; cached per bin.
func binVersion(bin string) string {
	if v, ok := versions.Load(bin); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "--version").Output()
	v := versionTok.FindString(string(out))
	if v == "" {
		v = "0.0.0"
	}
	versions.Store(bin, v)
	return v
}
