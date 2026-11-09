package syncer

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type ScanStats struct {
	ElapsedUS  int64
	Full       bool
	Visited    int
	Hashed     int
	Watched    int
	WatchError string
}
type watchState struct {
	mu      sync.Mutex
	watcher *fsnotify.Watcher
	dirty   map[string]bool
	full    bool
	watched map[string]bool
	failure string
}

func (s *Scanner) EnableWatch() {
	if s.watch != nil {
		return
	}
	w, err := fsnotify.NewWatcher()
	s.watch = &watchState{watcher: w, dirty: map[string]bool{}, watched: map[string]bool{}}
	if err != nil {
		s.watch.failure = err.Error()
		return
	}
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				rel, err := filepath.Rel(s.Root, ev.Name)
				if err != nil {
					continue
				}
				p := filepath.ToSlash(rel)
				if builtin(p) || p == ".." || strings.HasPrefix(p, "../") {
					continue
				}
				s.watch.mu.Lock()
				s.watch.dirty[p] = true
				if len(s.watch.dirty) > 65536 {
					s.watch.full = true
					clear(s.watch.dirty)
				}
				if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					delete(s.watch.watched, ev.Name)
				}
				s.watch.mu.Unlock()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				s.watch.mu.Lock()
				s.watch.full = true
				s.watch.failure = err.Error()
				s.watch.mu.Unlock()
			}
		}
	}()
}
func (s *Scanner) Close() error {
	if s.watch != nil && s.watch.watcher != nil {
		return s.watch.watcher.Close()
	}
	return nil
}
func (s *Scanner) addWatch(full string) {
	if s.watch == nil || s.watch.watcher == nil {
		return
	}
	s.watch.mu.Lock()
	if s.watch.watched[full] {
		s.watch.mu.Unlock()
		return
	}
	s.watch.mu.Unlock()
	err := s.watch.watcher.Add(full)
	s.watch.mu.Lock()
	defer s.watch.mu.Unlock()
	if err != nil {
		s.watch.failure = err.Error()
		return
	}
	s.watch.watched[full] = true
}
func (s *Scanner) Scan() (result Snapshot, scanErr error) {
	start := time.Now()
	defer func() { result.Stats.ElapsedUS = time.Since(start).Microseconds() }()
	s.Stats = ScanStats{}
	full := s.snapshot.Files == nil || s.watch == nil || time.Since(s.lastFull) >= 30*time.Second
	dirty := map[string]bool{}
	if s.watch != nil {
		s.watch.mu.Lock()
		for p := range s.watch.dirty {
			dirty[p] = true
		}
		s.watch.dirty = map[string]bool{}
		full = full || s.watch.full
		s.watch.full = false
		s.Stats.Watched = len(s.watch.watched)
		s.Stats.WatchError = s.watch.failure
		s.watch.mu.Unlock()
		if s.Stats.WatchError != "" && time.Since(s.lastFull) >= 2*time.Second {
			full = true
		}
	}
	for p := range dirty {
		if path.Base(p) == ".gitignore" || path.Base(p) == ".rsync_toolignore" {
			full = true
		}
	}
	if !full {
		for p := range dirty {
			if p == "." {
				continue
			}
			isDir := s.snapshot.Files[p].Dir
			if info, err := os.Lstat(filepath.Join(s.Root, filepath.FromSlash(p))); err == nil {
				isDir = info.IsDir()
			}
			if ignored(p, isDir, s.snapshot.Rules, s.Extra) {
				delete(dirty, p)
			}
		}
	}
	if !full && len(dirty) == 0 {
		s.snapshot.Stats = s.Stats
		return s.snapshot, nil
	}
	out := s.snapshot
	if full {
		out = Snapshot{Files: map[string]Entry{}, Rules: map[string]string{}}
		s.Stats.Full = true
	} else {
		out.Files = maps.Clone(out.Files)
		out.Rules = maps.Clone(out.Rules)
		out.Deferred = nil
	}
	blocked := map[string]bool{}
	if !full {
		for _, p := range out.Blocked {
			blocked[p] = true
		}
	}
	activeDirs := map[string]bool{}
	scan := func(prefix string) error {
		return filepath.WalkDir(filepath.Join(s.Root, filepath.FromSlash(prefix)), func(fullPath string, d fs.DirEntry, err error) error {
			rel, relErr := filepath.Rel(s.Root, fullPath)
			if relErr != nil {
				return relErr
			}
			p := filepath.ToSlash(rel)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			s.Stats.Visited++
			if p != "." && ((p == prefix && ignored(p, d.IsDir(), out.Rules, s.Extra)) || (p != prefix && ignoredLeaf(p, d.IsDir(), out.Rules, s.Extra))) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				activeDirs[fullPath] = true
				s.addWatch(fullPath)
				if p != "." {
					out.Files[p] = Entry{Dir: true, Mode: 0755}
				}
				for _, name := range []string{".gitignore", ".rsync_toolignore"} {
					if s.IgnoreRootGitignore && p == "." && name == ".gitignore" {
						continue
					}
					f := filepath.Join(fullPath, name)
					info, e := os.Lstat(f)
					if os.IsNotExist(e) {
						continue
					}
					if e != nil {
						return e
					}
					if !info.Mode().IsRegular() || info.Size() > s.Max {
						continue
					}
					b, e := os.ReadFile(f)
					if os.IsNotExist(e) {
						continue
					}
					if e != nil {
						return e
					}
					key := name
					if p != "." {
						key = path.Join(p, name)
					}
					out.Rules[key] = string(b)
				}
				return nil
			}
			info, e := d.Info()
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || info.Size() > s.Max {
				blocked[p] = true
				return nil
			}
			old, ok := s.cache[p]
			if ok && !dirty[p] && !old.Dir && old.Size == info.Size() && old.Mtime == info.ModTime().UnixNano() && old.Mode == uint32(info.Mode().Perm()) {
				out.Files[p] = old
				return nil
			}
			b, e := os.ReadFile(fullPath)
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			s.Stats.Hashed++
			after, e := os.Lstat(fullPath)
			if os.IsNotExist(e) {
				return nil
			}
			if e != nil {
				return e
			}
			if !after.Mode().IsRegular() || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
				out.Deferred = append(out.Deferred, p)
				if ok {
					out.Files[p] = old
				}
				if s.watch != nil {
					s.watch.mu.Lock()
					s.watch.dirty[p] = true
					s.watch.mu.Unlock()
				}
				return nil
			}
			out.Files[p] = Entry{Hash: digest(b), Size: info.Size(), Mode: uint32(info.Mode().Perm()), Mtime: info.ModTime().UnixNano()}
			return nil
		})
	}
	if full {
		if err := scan("."); err != nil {
			return Snapshot{}, err
		}
		s.lastFull = time.Now()
		if s.watch != nil && s.watch.watcher != nil {
			remove := []string{}
			s.watch.mu.Lock()
			for p := range s.watch.watched {
				if !activeDirs[p] {
					remove = append(remove, p)
					delete(s.watch.watched, p)
				}
			}
			s.watch.mu.Unlock()
			for _, p := range remove {
				s.watch.watcher.Remove(p)
			}
		}
	} else {
		paths := make([]string, 0, len(dirty))
		for p := range dirty {
			if p == "." || !ignored(p, false, out.Rules, s.Extra) {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		last := ""
		for _, p := range paths {
			if last != "" && (last == "." || strings.HasPrefix(p, last+"/")) {
				continue
			}
			last = p
			subtree := p == "." || s.snapshot.Files[p].Dir
			if info, err := os.Lstat(filepath.Join(s.Root, filepath.FromSlash(p))); err == nil && info.IsDir() {
				subtree = true
			}
			if !subtree {
				delete(out.Files, p)
				delete(blocked, p)
			} else {
				for old := range out.Files {
					if old == p || p == "." || strings.HasPrefix(old, p+"/") {
						delete(out.Files, old)
					}
				}
				for old := range blocked {
					if old == p || p == "." || strings.HasPrefix(old, p+"/") {
						delete(blocked, old)
					}
				}
				for old := range out.Rules {
					if p == "." || strings.HasPrefix(old, p+"/") {
						delete(out.Rules, old)
					}
				}
			}
			if err := scan(p); err != nil {
				if s.watch != nil {
					s.watch.mu.Lock()
					s.watch.full = true
					s.watch.mu.Unlock()
				}
				return Snapshot{}, err
			}
		}
	}
	out.Blocked = nil
	for p := range blocked {
		out.Blocked = append(out.Blocked, p)
	}
	sort.Strings(out.Blocked)
	out.Changed = nil
	for p, v := range out.Files {
		if old, ok := s.cache[p]; !ok || old != v {
			out.Changed = append(out.Changed, p)
		}
	}
	for p := range s.cache {
		if _, ok := out.Files[p]; !ok {
			out.Changed = append(out.Changed, p)
		}
	}
	s.revision++
	out.Revision = s.revision
	if s.watch != nil {
		s.watch.mu.Lock()
		s.Stats.Watched = len(s.watch.watched)
		s.Stats.WatchError = s.watch.failure
		s.watch.mu.Unlock()
	}
	out.Stats = s.Stats
	s.snapshot = out
	s.cache = out.Files
	return out, nil
}

// ForceScan is used by the final flush and tests to reconcile even if notification delivery is delayed.
func (s *Scanner) ForceScan() {
	if s.watch != nil {
		s.watch.mu.Lock()
		s.watch.full = true
		s.watch.mu.Unlock()
	}
}
func (s Snapshot) Busy(p string) bool {
	for _, b := range s.Deferred {
		if p == b || strings.HasPrefix(p, b+"/") {
			return true
		}
	}
	return false
}
