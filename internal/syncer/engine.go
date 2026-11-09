package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type State struct {
	Remote    string
	Base      map[string]Entry
	Conflicts map[string]string
	LastSync  time.Time
	LastError string
	PID       int
	Running   bool
}

func Load(root string) (State, error) {
	s := State{Base: map[string]Entry{}, Conflicts: map[string]string{}}
	b, e := os.ReadFile(filepath.Join(root, ".rsync_tool", "state.json"))
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	e = json.Unmarshal(b, &s)
	if s.Base == nil {
		s.Base = map[string]Entry{}
	}
	if s.Conflicts == nil {
		s.Conflicts = map[string]string{}
	}
	return s, e
}
func Save(root string, s State) error {
	dir := filepath.Join(root, ".rsync_tool")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, "state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), filepath.Join(dir, "state.json"))
}

type Engine struct {
	Local                         *Disk
	Remote                        Endpoint
	State                         *State
	Force                         string
	Log                           func(string, ...any)
	Changed                       bool
	initialized                   bool
	localRevision, remoteRevision uint64
}
type planned struct {
	p             string
	local, remote *Entry
	direction     string
	backup        bool
}

func signature(a, b *Entry) string { v, _ := json.Marshal([]*Entry{a, b}); return string(v) }
func (e *Engine) Cycle() (cycleErr error) {
	defer func() {
		if cycleErr != nil {
			e.initialized = false
		}
	}()
	e.Changed = false
	l, err := e.Local.Scan()
	if err != nil {
		return err
	}
	r, err := e.Remote.Scan()
	if err != nil {
		return err
	}
	if e.initialized && e.Force == "" && l.Revision == e.localRevision && r.Revision == e.remoteRevision {
		e.State.LastSync = time.Now()
		e.State.LastError = ""
		return nil
	}
	e.Changed = true
	defer func() {
		if cycleErr == nil {
			e.initialized = true
			e.localRevision = l.Revision
			e.remoteRevision = r.Revision
		} else {
			e.initialized = false
		}
	}()
	pending := map[string]string{}
	all := map[string]bool{}
	if e.initialized && e.Force == "" && !(l.Revision != e.localRevision && l.Stats.Full) && !(r.Revision != e.remoteRevision && r.Stats.Full) {
		if l.Revision != e.localRevision {
			for _, p := range l.Changed {
				all[p] = true
			}
		}
		if r.Revision != e.remoteRevision {
			for _, p := range r.Changed {
				all[p] = true
			}
		}
	} else {
		for p := range l.Files {
			all[p] = true
		}
		for p := range r.Files {
			all[p] = true
		}
		for p := range e.State.Base {
			all[p] = true
		}
	}
	for _, p := range l.Deferred {
		all[p] = true
	}
	for _, p := range r.Deferred {
		all[p] = true
	}
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	plans := []planned{}
	localFilter, remoteFilter := NewFilter(l, e.Local.Scanner.Extra), NewFilter(r, e.Local.Scanner.Extra)
	for _, p := range paths {
		if l.Busy(p) || r.Busy(p) {
			pending[p] = "file is being written"
			continue
		}
		directory := false
		for _, v := range []*Entry{ptr(l.Files, p), ptr(r.Files, p), ptr(e.State.Base, p)} {
			if v != nil && v.Dir {
				directory = true
			}
		}
		if !localFilter.Allows(p, directory) || !remoteFilter.Allows(p, directory) {
			delete(e.State.Base, p)
			delete(e.State.Conflicts, p)
			continue
		}
		a, b, base := ptr(l.Files, p), ptr(r.Files, p), ptr(e.State.Base, p)
		if Same(a, b) {
			if a == nil {
				delete(e.State.Base, p)
			} else {
				e.State.Base[p] = *a
			}
			delete(e.State.Conflicts, p)
			continue
		}
		direction := e.Force
		backup := direction != ""
		if direction == "" {
			lc, rc := !Same(a, base), !Same(b, base)
			switch {
			case lc && !rc:
				direction = "push"
			case rc && !lc:
				direction = "pull"
			default:
				sig := signature(a, b)
				if e.State.Conflicts[p] != sig {
					lf, err := e.Local.Read([]string{p})
					if err != nil {
						return err
					}
					rf, err := e.Remote.Read([]string{p})
					if err != nil {
						return err
					}
					if len(lf) != 1 || len(rf) != 1 || lf[0].Error != "" || rf[0].Error != "" || !Same(lf[0].Entry, a) || !Same(rf[0].Entry, b) {
						pending[p] = "file changed while preserving conflict"
						continue
					}
					if err = saveConflict(e.Local.Scanner.Root, "local", lf[0]); err != nil {
						return err
					}
					if err = saveConflict(e.Local.Scanner.Root, "remote", rf[0]); err != nil {
						return err
					}
					e.State.Conflicts[p] = sig
					e.Log("conflict: %s (use -f pull or -f push)", p)
				}
				continue
			}
		}
		plans = append(plans, planned{p, a, b, direction, backup})
	}
	// Directories first for creation, children first for deletion.
	sort.SliceStable(plans, func(i, j int) bool {
		rank := func(p planned) int {
			src, dst := p.local, p.remote
			if p.direction == "pull" {
				src, dst = p.remote, p.local
			}
			if src != nil {
				if src.Dir {
					return 0
				}
				return 1
			}
			if dst != nil && dst.Dir {
				return 3
			}
			return 2
		}
		a, b := rank(plans[i]), rank(plans[j])
		if a != b {
			return a < b
		}
		if a == 3 {
			return len(plans[i].p) > len(plans[j].p)
		}
		return plans[i].p < plans[j].p
	})
	// Bound each batch to 8 MiB; send all files in the batch in one RPC.
	for _, direction := range []string{"pull", "push"} {
		for i := 0; i < len(plans); {
			batch := []planned{}
			var size int64
			for i < len(plans) {
				p := plans[i]
				i++
				if p.direction != direction {
					continue
				}
				batch = append(batch, p)
				source := p.local
				if direction == "pull" {
					source = p.remote
				}
				if source != nil {
					size += source.Size
				}
				if size >= 8*1024*1024 || len(batch) >= 512 {
					break
				}
			}
			if len(batch) == 0 {
				continue
			}
			ps := []string{}
			for _, p := range batch {
				ps = append(ps, p.p)
			}
			var src, dst Endpoint = e.Local, e.Remote
			if direction == "pull" {
				src, dst = e.Remote, e.Local
			}
			files, err := src.Read(ps)
			if err != nil {
				return err
			}
			if len(files) != len(batch) {
				return fmt.Errorf("invalid read response")
			}
			changes := []Change{}
			for n, p := range batch {
				want, expected := p.local, p.remote
				if direction == "pull" {
					want, expected = p.remote, p.local
				}
				if files[n].Path != p.p {
					return fmt.Errorf("invalid read response path")
				}
				if files[n].Error != "" || !Same(files[n].Entry, want) {
					pending[p.p] = "file changed during transfer"
					continue
				}
				changes = append(changes, Change{files[n], expected, p.backup})
			}
			failures, err := dst.Apply(changes)
			if err != nil {
				return err
			}
			for n, p := range batch {
				if _, skip := pending[p.p]; skip {
					continue
				}
				if _, failed := failures[p.p]; failed {
					continue
				}
				v := files[n].Entry
				if v == nil {
					delete(e.State.Base, p.p)
				} else {
					e.State.Base[p.p] = *v
				}
				delete(e.State.Conflicts, p.p)
				e.Log("%s %s", direction, p.p)
			}
			if len(failures) > 0 {
				for p, why := range failures {
					pending[p] = why
				}
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d paths deferred for retry: %v", len(pending), pending)
	}
	e.State.LastSync = time.Now()
	e.State.LastError = ""
	return nil
}
