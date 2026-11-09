package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	ignore "github.com/sabhiram/go-gitignore"
)

const DefaultMax int64 = 2 * 1024 * 1024

type ignoreRule struct {
	matcher *ignore.GitIgnore
	negate  bool
}

var ruleCache = struct {
	sync.Mutex
	items map[string][]ignoreRule
}{items: map[string][]ignoreRule{}}

func applyRules(content, rel string, result bool) bool {
	ruleCache.Lock()
	rules, ok := ruleCache.items[content]
	if !ok {
		for _, line := range strings.Split(content, "\n") {
			negate := strings.HasPrefix(line, "!")
			if negate {
				line = strings.TrimPrefix(line, "!")
			}
			rules = append(rules, ignoreRule{ignore.CompileIgnoreLines(line), negate})
		}
		if len(ruleCache.items) >= 256 {
			ruleCache.items = map[string][]ignoreRule{}
		}
		ruleCache.items[content] = rules
	}
	ruleCache.Unlock()
	for _, rule := range rules {
		if rule.matcher.MatchesPath(rel) {
			result = !rule.negate
		}
	}
	return result
}

type Entry struct {
	Hash  string
	Size  int64
	Mode  uint32
	Mtime int64
	Dir   bool `json:",omitempty"`
}

func Same(a, b *Entry) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Hash == b.Hash && a.Dir == b.Dir && (a.Dir || a.Mode == b.Mode)
}

type Snapshot struct {
	Files    map[string]Entry
	Rules    map[string]string
	Blocked  []string
	Changed  []string `json:",omitempty"`
	Deferred []string
	Revision uint64
	Stats    ScanStats
}
type Scanner struct {
	Root                string
	Max                 int64
	Extra               []string
	cache               map[string]Entry
	IgnoreRootGitignore bool
	watch               *watchState
	snapshot            Snapshot
	lastFull            time.Time
	revision            uint64
	Stats               ScanStats
}

func NewScanner(root string, max int64, extra []string) *Scanner {
	return &Scanner{Root: root, Max: max, Extra: extra, cache: map[string]Entry{}}
}
func builtin(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == ".git" || s == ".rsync_tool" || strings.HasPrefix(s, ".rsync_tool-tmp-") {
			return true
		}
	}
	return false
}
func ignoredLeaf(p string, dir bool, rules map[string]string, extra []string) bool {
	if builtin(p) {
		return true
	}
	parts := strings.Split(p, "/")
	result := false
	for depth := 0; depth < len(parts); depth++ {
		scope := strings.Join(parts[:depth], "/")
		rel := strings.Join(parts[depth:], "/")
		if dir {
			rel += "/"
		}
		for _, name := range []string{".gitignore", ".rsync_toolignore"} {
			if content, ok := rules[path.Join(scope, name)]; ok {
				result = applyRules(content, rel, result)
			}
		}
	}
	rel := p
	if dir {
		rel += "/"
	}
	if len(extra) > 0 {
		result = applyRules(strings.Join(extra, "\n"), rel, result)
	}
	return result
}
func ignored(p string, dir bool, rules map[string]string, extra []string) bool {
	parts := strings.Split(p, "/")
	for n := 1; n <= len(parts); n++ {
		if ignoredLeaf(strings.Join(parts[:n], "/"), n < len(parts) || dir, rules, extra) {
			return true
		}
	}
	return false
}

// Filter memoizes directory decisions while planning a batch of file changes.
type Filter struct {
	snapshot Snapshot
	extra    []string
	dirs     map[string]bool
}

func NewFilter(s Snapshot, extra []string) *Filter {
	return &Filter{s, extra, map[string]bool{".": true}}
}
func (f *Filter) Allows(p string, dir bool) bool {
	if dir {
		if result, ok := f.dirs[p]; ok {
			return result
		}
	}
	parent := path.Dir(p)
	result := f.AllowsParent(parent) && !ignoredLeaf(p, dir, f.snapshot.Rules, f.extra)
	if result {
		for q := p; q != "." && q != "/"; q = path.Dir(q) {
			i := sort.SearchStrings(f.snapshot.Blocked, q)
			if i < len(f.snapshot.Blocked) && f.snapshot.Blocked[i] == q {
				result = false
				break
			}
		}
	}
	if dir {
		f.dirs[p] = result
	}
	return result
}
func (f *Filter) AllowsParent(p string) bool {
	if p == "." || p == "/" {
		return true
	}
	return f.Allows(p, true)
}
func (s Snapshot) Allows(p string, extra []string, directory ...bool) bool {
	dir := false
	if len(directory) > 0 {
		dir = directory[0]
	}
	if ignored(p, dir, s.Rules, extra) {
		return false
	}
	for q := p; q != "." && q != "/"; q = path.Dir(q) {
		i := sort.SearchStrings(s.Blocked, q)
		if i < len(s.Blocked) && s.Blocked[i] == q {
			return false
		}
	}
	return true
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// safePath rejects symlink ancestors and traversal before any read or write.
func safePath(root, p string) (string, error) {
	if p == "" || p == "." || path.Clean(p) != p || strings.Contains(p, "\\") || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || builtin(p) {
		return "", fmt.Errorf("unsafe path: %q", p)
	}
	full := root
	for _, part := range strings.Split(p, "/") {
		full = filepath.Join(full, part)
		st, e := os.Lstat(full)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink path: %s", p)
		}
	}
	return full, nil
}
func current(root, p string, max int64) (*Entry, []byte, error) {
	full, e := safePath(root, p)
	if e != nil {
		return nil, nil, e
	}
	st, e := os.Lstat(full)
	if os.IsNotExist(e) {
		return nil, nil, nil
	}
	if e != nil {
		return nil, nil, e
	}
	if st.IsDir() {
		return &Entry{Dir: true, Mode: 0755}, nil, nil
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, nil, fmt.Errorf("ineligible destination: %s", p)
	}
	b, e := os.ReadFile(full)
	if e != nil {
		return nil, nil, e
	}
	return &Entry{Hash: digest(b), Size: st.Size(), Mode: uint32(st.Mode().Perm()), Mtime: st.ModTime().UnixNano()}, b, nil
}

type File struct {
	Path  string
	Entry *Entry
	Data  []byte
	Error string `json:",omitempty"`
}
type Change struct {
	File     File
	Expected *Entry
	Backup   bool
}

func apply(root string, max int64, c Change) error {
	full, e := safePath(root, c.File.Path)
	if e != nil {
		return e
	}
	now, b, e := current(root, c.File.Path, max)
	if e != nil {
		return e
	}
	if !Same(now, c.Expected) {
		return fmt.Errorf("concurrent change: %s", c.File.Path)
	}
	if c.Backup && now != nil {
		if e = saveConflict(root, "overwritten", File{Path: c.File.Path, Entry: now, Data: b}); e != nil {
			return e
		}
	}
	if c.File.Entry == nil {
		if now == nil {
			return nil
		}
		return os.Remove(full)
	}
	if c.File.Entry.Dir {
		if now != nil && !now.Dir {
			return fmt.Errorf("file/directory replacement requires explicit removal: %s", c.File.Path)
		}
		return os.MkdirAll(full, 0755)
	}
	if int64(len(c.File.Data)) > max || digest(c.File.Data) != c.File.Entry.Hash {
		return fmt.Errorf("invalid payload: %s", c.File.Path)
	}
	if e = os.MkdirAll(filepath.Dir(full), 0755); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(full), ".rsync_tool-tmp-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(c.File.Data); e != nil {
		f.Close()
		return e
	}
	if e = f.Chmod(fs.FileMode(c.File.Entry.Mode) & 0777); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	// Recheck after creating the temporary file, immediately before replacement.
	again, _, e := current(root, c.File.Path, max)
	if e != nil {
		return e
	}
	if !Same(again, c.Expected) {
		return fmt.Errorf("concurrent change: %s", c.File.Path)
	}
	if e = os.Rename(tmp, full); e != nil {
		return e
	}
	t := time.Unix(0, c.File.Entry.Mtime)
	return os.Chtimes(full, t, t)
}
func saveConflict(root, side string, f File) error {
	if f.Entry == nil || f.Entry.Dir {
		return nil
	}
	if _, e := safePath(root, f.Path); e != nil {
		return e
	}
	p := filepath.Join(root, ".rsync_tool", "conflicts", f.Path+"."+side+"."+f.Entry.Hash[:16])
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	return os.WriteFile(p, f.Data, 0600)
}
func keys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func ptr(m map[string]Entry, p string) *Entry {
	v, ok := m[p]
	if !ok {
		return nil
	}
	return &v
}
