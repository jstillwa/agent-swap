package swap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Mirror is agent-swap's view of one harness kind's session for this run.
type Mirror struct {
	Native, Path string
	Cursor       string // byte offset (decimal) for JSONL kinds; last msg_ id for opencode
	Synced       int    // log index already present in the session
	Base         int    // log index where its last Create started (budget)
	Dev, Ino     uint64 // identity recorded when written or read
	Owned        bool   // created during this run (by agent-swap or the harness)
	seen         int64  // file size at the last ingest (unread)
}

// conv is the run's conversation: the log and one mirror per harness kind.
// The runner is its only user; nothing of it is persisted.
type conv struct {
	log     []Turn
	mirrors map[string]*Mirror
	sent    map[string]bool // prompts agent-swap typed (clean form): not conversation
	paused  map[string]bool // kinds whose session changed to one we rejected
	forked  bool            // claude's first launch got --fork-session
	resumed string          // the session id that fork started from (not ours)
	foreign bool            // the first launch resumes a pre-existing session
}

func (c *conv) mirror(kind string) *Mirror {
	if c.mirrors[kind] == nil {
		c.mirrors[kind] = &Mirror{Synced: len(c.log), Base: len(c.log)}
	}
	return c.mirrors[kind]
}

// add appends a harness's turns to the log, minus agent-swap's own prompts.
func (c *conv) add(kind string, turns []Turn) {
	for _, t := range turns {
		if t.Text == "" || t.Role == "user" && c.sent[t.Text] {
			continue
		}
		t.Origin = kind
		c.log = append(c.log, t)
	}
}

// ingest reads the running harness's newly completed turns into the log
// (with rt.Final after the child exited: also its trailing incomplete turn).
// agy is read only at exit (its transcript is a lossy whole).
func (r *Runner) ingest(h Harness, ad Adapter, rt Runtime, s Session, exit bool) {
	k := h.Kind
	if s.NativeID == "" || r.c.paused[k] || k == "agy" && !exit {
		return
	}
	m := r.c.mirror(k)
	switch {
	case m.Native == "":
		p, err := r.verify(k, ad, s.NativeID, rt, false)
		if err != nil {
			return
		}
		r.adopt(k, m, s.NativeID, p, false)
	case s.NativeID != m.Native:
		r.epoch(k, ad, rt, s.NativeID)
		return
	}
	turns, cur, err := ad.Turns(m.Native, m.Cursor, rt)
	if errors.Is(err, errRewound) {
		r.Log.Printf("sync: %s rewound before cursor; earlier mirrored turns not retracted", k)
	} else if err != nil {
		r.Log.Printf("ingest %s: %v", h.Name, err)
		return
	}
	if fi, err := os.Stat(m.Path); m.Path != "" && err == nil {
		m.seen = fi.Size()
	}
	if len(turns) == 0 && cur == m.Cursor {
		return
	}
	n := len(r.c.log)
	r.c.add(k, turns)
	if n < len(r.c.log) {
		r.Log.Printf("ingest %s: +%d turns", h.Name, len(r.c.log)-n)
	}
	m.Cursor, m.Synced = cur, len(r.c.log)
	r.saveMirrors()
}

// unread reports whether the kind's session file changed size since the last ingest.
func (r *Runner) unread(kind string) bool {
	m := r.c.mirrors[kind]
	if m == nil || m.Path == "" || r.c.paused[kind] {
		return false
	}
	fi, err := os.Stat(m.Path)
	return err == nil && fi.Size() != m.seen
}

// verify locates native's session and checks it belongs to this cwd: the
// recorded cwd (opencode: its project directory) must be rt.Cwd, and with
// strict a claude file must be in the cwd's own project dir. A session not
// written yet (os.ErrNotExist) is retried later; any other failure pauses
// sync from that kind.
func (r *Runner) verify(k string, ad Adapter, native string, rt Runtime, strict bool) (string, error) {
	path, cwd, err := "", rt.Cwd, error(nil)
	if _, ok := ad.(opencodeAdapter); ok {
		if cwd, err = ocSessionDir(native, rt); err != nil {
			err = fmt.Errorf("%w: %v", os.ErrNotExist, err)
		}
	} else if storeRoot(k, rt) != "" {
		if path, err = sessionPath(k, native, rt); err == nil {
			if cwd = sessionCwd(path); cwd == "" {
				err = fmt.Errorf("%s: no cwd recorded yet: %w", path, os.ErrNotExist)
			} else if strict && k == "claude" && filepath.Dir(path) != filepath.Join(storeRoot(k, rt), "projects", claudeSlug(rt.Cwd)) {
				err = fmt.Errorf("%s is not in this cwd's project dir", path)
			}
		}
	}
	if err == nil && !sameDir(cwd, rt.Cwd) {
		err = fmt.Errorf("session %s is for %q, not %q", native, cwd, rt.Cwd)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.Log.Printf("sync: %s session %s not ready (%v); retrying", k, native, err)
	case err != nil:
		r.c.paused[k] = true
		r.Log.Printf("sync paused: %s session %s rejected: %v", k, native, err)
	}
	return path, err
}

// adopt makes native (at path, "" for opencode) the kind's session. It is
// owned (appendable) when it was created during this run: a JSONL file whose
// first entry is not older than the run, claude's forked first session, or an
// opencode session that the first launch did not resume.
func (r *Runner) adopt(k string, m *Mirror, native, path string, epoch bool) {
	m.Native, m.Path, m.Dev, m.Ino, m.Owned = native, "", 0, 0, false
	if path == "" {
		m.Owned = !epoch && !r.c.foreign // opencode (agy never gets written)
	} else if st, err := regular(path); err == nil {
		m.Path, m.Dev, m.Ino = path, uint64(st.Dev), st.Ino
		m.Owned = bornAfter(path, r.started) || !epoch && k == "claude" && r.c.forked && native != r.c.resumed
	}
	r.Log.Printf("session %s: %s owned=%v %s", k, native, m.Owned, m.Path)
}

// epoch handles the running harness switching to another native session
// (/clear, /new, /resume...). An acceptable session (see verify) restarts the
// conversation from it and drops the other mirrors.
func (r *Runner) epoch(k string, ad Adapter, rt Runtime, native string) {
	path, err := r.verify(k, ad, native, rt, true)
	if err != nil {
		return
	}
	turns, cur, err := ad.Turns(native, "", rt)
	if err != nil && !errors.Is(err, errRewound) {
		r.c.paused[k] = true
		r.Log.Printf("sync paused: %s session %s unreadable: %v", k, native, err)
		return
	}
	r.Log.Printf("epoch: %s switched to session %s; the conversation restarts from it", k, native)
	r.c.log = nil
	r.c.add(k, turns)
	m := &Mirror{Cursor: cur, Synced: len(r.c.log)}
	r.adopt(k, m, native, path, true)
	r.c.mirrors = map[string]*Mirror{k: m}
	r.saveMirrors()
}

// materialize writes the log into harness h's session before it takes over
// (or on `sync`): an Append of the turns it lacks to an owned, intact session
// within budget, else a Create of the capped log, retried once.
func (r *Runner) materialize(h Harness, rt Runtime) error {
	k, log := h.Kind, r.c.log
	if len(log) == 0 {
		return nil
	}
	if k == "agy" { // protobuf store: it gets the context file
		return errNoMirror
	}
	ad, err := AdapterFor(k)
	if err != nil {
		return err
	}
	budget := orDefault(h.ContextMaxBytes, r.Cfg.ContextMaxBytes)
	if m := r.c.mirrors[k]; m != nil && m.Native != "" {
		var delta []Turn
		for _, t := range log[min(m.Synced, len(log)):] {
			if t.Origin != k { // its own turns are already native there
				delta = append(delta, t)
			}
		}
		if len(delta) == 0 { // e.g. a claude billing swap: resume as is, owned or not
			m.Synced = len(log)
			return nil
		}
		if !m.Owned {
			err = errors.New("not created during this run")
		} else if logBytes(log[min(m.Base, len(log)):]) > budget {
			err = errors.New("over budget")
		} else if err = checkOwned(storeRoot(k, rt), m); err == nil {
			if err = r.write(ad, h, m, merge(delta), rt); err == nil {
				m.Synced = len(log)
				r.Log.Printf("sync: %s appended %d turns to %s", h.Name, len(delta), m.Native)
				r.saveMirrors()
				return nil
			}
		}
		r.Log.Printf("sync: %s cannot append to %s (%v); creating a session", h.Name, m.Native, err)
	}
	kept, omitted := capTurns(log, budget)
	if omitted > 0 {
		kept = append([]Turn{omittedNote(omitted)}, kept...)
	}
	for try := 0; try < 2 && !errors.Is(err, errNoMirror); try++ {
		m := &Mirror{}
		if err = r.write(ad, h, m, merge(kept), rt); err == nil {
			m.Owned, m.Synced, m.Base = true, len(log), omitted
			r.c.mirrors[k] = m
			r.Log.Printf("sync: %s created session %s (%d turns, %d omitted) %s", h.Name, m.Native, len(log)-omitted, omitted, m.Path)
			r.saveMirrors()
			return nil
		}
		r.Log.Printf("sync: %s create: %v", h.Name, err)
	}
	return err
}

// write runs the adapter's Write, then reads the session back from the old
// cursor: it must parse and hold at least the turns Write says are readable.
func (r *Runner) write(ad Adapter, h Harness, m *Mirror, turns []Turn, rt Runtime) error {
	saved := *m
	want, err := ad.Write(h, m, turns, rt)
	if err != nil {
		return err
	}
	got, _, err := ad.Turns(m.Native, saved.Cursor, rt)
	switch {
	case err != nil && !errors.Is(err, errRewound):
		err = fmt.Errorf("self-check: %w", err)
	case len(got) < want:
		err = fmt.Errorf("self-check: read back %d of %d turns", len(got), want)
	default:
		return nil
	}
	*m = saved // a failed Append must not advance the cursor
	return err
}

// takeover prepares harness h to take over: its session is brought up to
// date, or it gets a context file. It returns the extra prompt to launch with.
func (r *Runner) takeover(h Harness, from string, rt Runtime, n int) string {
	err := r.materialize(h, rt)
	if err == nil {
		return ""
	}
	kept, omitted := capTurns(r.c.log, orDefault(h.ContextMaxBytes, r.Cfg.ContextMaxBytes))
	file := filepath.Join(r.Dir, fmt.Sprintf("context-%d.md", n))
	_, _, werr := createExcl(r.Dir, file, []byte(markdown(kept, omitted)))
	r.Log.Printf("sync: context file (%v)", err)
	// It launches fresh; its new session holds everything up to here.
	r.c.mirrors[h.Kind] = &Mirror{Synced: len(r.c.log), Base: len(r.c.log)}
	r.saveMirrors()
	if werr != nil {
		r.Log.Printf("context file: %v", werr)
		return ""
	}
	return Render(r.Cfg.ResumeMessage, map[string]string{"context": file, "from": from, "to": h.Name})
}

// saveMirrors writes mirrors.json for `status`; the runner never reads it.
func (r *Runner) saveMirrors() {
	st := map[string]MirrorStatus{}
	for k, m := range r.c.mirrors {
		if m.Native == "" {
			continue
		}
		behind := 0
		for _, t := range r.c.log[min(m.Synced, len(r.c.log)):] {
			if t.Origin != k {
				behind++
			}
		}
		st[k] = MirrorStatus{Native: m.Native, Path: m.Path, Behind: behind}
	}
	if err := Update(MirrorsPath(r.Dir), func(v *map[string]MirrorStatus) { *v = st }); err != nil {
		r.Log.Printf("mirrors.json: %v", err)
	}
}

// sameDir compares two directory paths after resolving symlinks (macOS
// /var vs /private/var); unresolvable paths compare as given.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
