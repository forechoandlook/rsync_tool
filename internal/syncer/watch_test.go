package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func watched(t *testing.T, root string) *Scanner {
	t.Helper()
	s := NewScanner(root, DefaultMax, nil)
	s.EnableWatch()
	t.Cleanup(func() { s.Close() })
	return s
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("notification/sync did not converge")
}
func TestWatchIncrementalAndRename(t *testing.T) {
	root := t.TempDir()
	put(t, root, "before/deep/a", "one")
	s := watched(t, root)
	initial, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if initial.Stats.WatchError != "" {
		t.Skip(initial.Stats.WatchError)
	}
	// Let initial registration settle, then verify idle scans perform no traversal.
	time.Sleep(20 * time.Millisecond)
	s.Scan()
	idle, err := s.Scan()
	if err != nil || idle.Stats.Visited != 0 {
		t.Fatalf("idle traversed: %+v %v", idle.Stats, err)
	}
	put(t, root, "before/deep/a", "two")
	eventually(t, func() bool {
		out, e := s.Scan()
		return e == nil && out.Files["before/deep/a"].Hash == digest([]byte("two"))
	})
	if err = os.Rename(filepath.Join(root, "before"), filepath.Join(root, "after")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		out, e := s.Scan()
		_, old := out.Files["before/deep/a"]
		return e == nil && !old && out.Files["after/deep/a"].Hash == digest([]byte("two"))
	})
	put(t, root, "after/deep/a", "three")
	eventually(t, func() bool {
		out, e := s.Scan()
		return e == nil && out.Files["after/deep/a"].Hash == digest([]byte("three"))
	})
	if err = os.RemoveAll(filepath.Join(root, "after")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { out, e := s.Scan(); return e == nil && len(out.Files) == 0 })
}
func TestWatchNewSubtreeAndIgnoreChanges(t *testing.T) {
	root := t.TempDir()
	s := watched(t, root)
	s.Scan()
	put(t, root, "new/deep/a", "yes")
	put(t, root, "new/deep/b", "yes")
	eventually(t, func() bool {
		out, e := s.Scan()
		return e == nil && out.Files["new/deep/a"].Hash != "" && out.Files["new/deep/b"].Hash != ""
	})
	put(t, root, "new/.gitignore", "deep/\n")
	eventually(t, func() bool {
		out, e := s.Scan()
		_, exists := out.Files["new/deep/a"]
		return e == nil && !exists && out.Rules["new/.gitignore"] == "deep/\n"
	})
	put(t, root, "new/.gitignore", "")
	eventually(t, func() bool { out, e := s.Scan(); return e == nil && out.Files["new/deep/a"].Hash != "" })
}
func TestEmptyDirectoriesAndRecursiveRemoval(t *testing.T) {
	e, l, r := setup(t)
	if err := os.MkdirAll(filepath.Join(r, "empty/nested"), 0755); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if st, err := os.Stat(filepath.Join(l, "empty/nested")); err != nil || !st.IsDir() {
		t.Fatal("empty directory not pulled")
	}
	put(t, l, "empty/nested/a", "a")
	cycle(t, e)
	if err := os.RemoveAll(filepath.Join(l, "empty")); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if _, err := os.Stat(filepath.Join(r, "empty")); !os.IsNotExist(err) {
		t.Fatal("directory not removed")
	}
}
func TestIgnoredDirectoryAfterTracking(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "dir/a", "keep")
	cycle(t, e)
	put(t, l, ".gitignore", "dir/\n")
	cycle(t, e)
	cycle(t, e)
	if get(t, r, "dir/a") != "keep" {
		t.Fatal("new ignore deleted tracked directory")
	}
}
func TestIgnoreRootOnly(t *testing.T) {
	root := t.TempDir()
	put(t, root, ".gitignore", "project/\n")
	put(t, root, "project/.gitignore", "*.tmp\n")
	put(t, root, "project/a", "yes")
	put(t, root, "project/a.tmp", "no")
	s := NewScanner(root, DefaultMax, nil)
	s.IgnoreRootGitignore = true
	out, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if out.Files["project/a"].Hash == "" {
		t.Fatal("root ignore not skipped")
	}
	if _, ok := out.Files["project/a.tmp"]; ok {
		t.Fatal("nested ignore not applied")
	}
}

type flakyRead struct {
	*Disk
	fail bool
}

func (r *flakyRead) Read(ps []string) ([]File, error) {
	files, err := r.Disk.Read(ps)
	if r.fail {
		for i := range files {
			if files[i].Path == "a" {
				files[i].Error = "busy"
			}
		}
	}
	return files, err
}
func TestBusyFileDoesNotBlockOtherFiles(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "a", "a")
	put(t, r, "b", "b")
	remote := &flakyRead{e.Remote.(*Disk), true}
	e.Remote = remote
	if err := e.Cycle(); err == nil || IsTransportError(err) {
		t.Fatalf("expected application retry, got %v", err)
	}
	if get(t, l, "b") != "b" {
		t.Fatal("unrelated file blocked")
	}
	if _, err := os.Stat(filepath.Join(l, "a")); !os.IsNotExist(err) {
		t.Fatal("busy file copied")
	}
	remote.fail = false
	cycle(t, e)
	if get(t, l, "a") != "a" {
		t.Fatal("busy file not retried")
	}
}
func TestDirectoryRemovalPreservesIgnoredContents(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, ".gitignore", "*.cache\n")
	put(t, r, "dir/a", "a")
	cycle(t, e)
	put(t, l, "dir/private.cache", "private")
	os.RemoveAll(filepath.Join(r, "dir"))
	put(t, r, "other", "sync me")
	err := e.Cycle()
	if err == nil || IsTransportError(err) {
		t.Fatalf("expected nonempty-directory retry, got %v", err)
	}
	if get(t, l, "dir/private.cache") != "private" || get(t, l, "other") != "sync me" {
		t.Fatal("ignored content removed or other path blocked")
	}
}

func benchmarkFixture(b *testing.B) *Scanner {
	root := b.TempDir()
	for d := 0; d < 40; d++ {
		dir := filepath.Join(root, fmt.Sprintf("d%03d", d))
		if err := os.MkdirAll(dir, 0755); err != nil {
			b.Fatal(err)
		}
		for n := 0; n < 100; n++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.go", n)), []byte(strings.Repeat("package p\n", 10)), 0644); err != nil {
				b.Fatal(err)
			}
		}
	}
	return NewScanner(root, DefaultMax, nil)
}
func BenchmarkScannerIdle4000(b *testing.B) {
	s := benchmarkFixture(b)
	s.EnableWatch()
	defer s.Close()
	if _, err := s.Scan(); err != nil {
		b.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	s.Scan()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := s.Scan(); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkScannerFull4000(b *testing.B) {
	s := benchmarkFixture(b)
	if _, err := s.Scan(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := s.Scan(); err != nil {
			b.Fatal(err)
		}
	}
}

type busySnapshot struct{ *Disk }

func (d *busySnapshot) Scan() (Snapshot, error) {
	s, err := d.Disk.Scan()
	s.Deferred = []string{"new-file-being-written"}
	return s, err
}
func TestNewBusyFilePreventsFalseFlushSuccess(t *testing.T) {
	e, _, _ := setup(t)
	e.Remote = &busySnapshot{e.Remote.(*Disk)}
	if err := e.Cycle(); err == nil || IsTransportError(err) {
		t.Fatalf("expected pending new file, got %v", err)
	}
}
func TestWatchMetadataPreservingRewrite(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a", "before")
	s := watched(t, root)
	s.Scan()
	info, err := os.Stat(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "a", "after!")
	if err = os.Chtimes(filepath.Join(root, "a"), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { out, e := s.Scan(); return e == nil && out.Files["a"].Hash == digest([]byte("after!")) })
}
