package syncer

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, root, p, s string) {
	t.Helper()
	f := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(f), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}
func get(t *testing.T, root, p string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(root, p))
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func setup(t *testing.T) (*Engine, string, string) {
	t.Helper()
	l, r := t.TempDir(), t.TempDir()
	s := State{Base: map[string]Entry{}, Conflicts: map[string]string{}}
	return &Engine{Local: &Disk{NewScanner(l, DefaultMax, nil)}, Remote: &Disk{NewScanner(r, DefaultMax, nil)}, State: &s, Log: func(string, ...any) {}}, l, r
}
func cycle(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Cycle(); err != nil {
		t.Fatal(err)
	}
}
func TestBidirectionalAndDelete(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "src/main.go", "remote")
	cycle(t, e)
	if get(t, l, "src/main.go") != "remote" {
		t.Fatal("initial pull")
	}
	put(t, l, "src/main.go", "agent change")
	put(t, l, "new.txt", "new")
	cycle(t, e)
	if get(t, r, "src/main.go") != "agent change" || get(t, r, "new.txt") != "new" {
		t.Fatal("push")
	}
	put(t, r, "new.txt", "remote edit")
	cycle(t, e)
	if get(t, l, "new.txt") != "remote edit" {
		t.Fatal("remote edit")
	}
	if err := os.Remove(filepath.Join(l, "new.txt")); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if _, err := os.Stat(filepath.Join(r, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("delete not propagated")
	}
}
func TestConflictAndForcedResolution(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "a", "base")
	cycle(t, e)
	put(t, l, "a", "left")
	put(t, r, "a", "right")
	cycle(t, e)
	if len(e.State.Conflicts) != 1 || get(t, l, "a") != "left" || get(t, r, "a") != "right" {
		t.Fatal("conflict overwritten")
	}
	copies, err := filepath.Glob(filepath.Join(l, ".rsync_tool/conflicts/a.*"))
	if err != nil || len(copies) != 2 {
		t.Fatalf("conflict copies %v %v", copies, err)
	}
	e.Force = "pull"
	cycle(t, e)
	if get(t, l, "a") != "right" || len(e.State.Conflicts) != 0 {
		t.Fatal("forced pull failed")
	}
	put(t, l, "a", "forced local")
	put(t, r, "a", "forced remote")
	e.Force = "push"
	cycle(t, e)
	if get(t, r, "a") != "forced local" {
		t.Fatal("forced push failed")
	}
	copies, _ = filepath.Glob(filepath.Join(r, ".rsync_tool/conflicts/a.overwritten.*"))
	if len(copies) != 1 {
		t.Fatal("remote force backup missing")
	}
}
func TestDeleteModifyConflict(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "a", "base")
	cycle(t, e)
	os.Remove(filepath.Join(l, "a"))
	put(t, r, "a", "remote change")
	cycle(t, e)
	if len(e.State.Conflicts) != 1 {
		t.Fatal("expected delete/modify conflict")
	}
	e.Force = "push"
	cycle(t, e)
	if _, err := os.Stat(filepath.Join(r, "a")); !os.IsNotExist(err) {
		t.Fatal("force deletion")
	}
}
func TestNestedIgnoreSizeAndSymlink(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, ".gitignore", "*.log\n")
	put(t, r, ".git/config", "secret")
	put(t, r, "sub/.gitignore", "!keep.log\ncache/\n")
	put(t, r, "sub/keep.log", "yes")
	put(t, r, "sub/drop.log", "no")
	put(t, r, "sub/cache/a", "no")
	put(t, r, "big", strings.Repeat("x", int(DefaultMax)+1))
	put(t, r, "exact", strings.Repeat("e", int(DefaultMax)))
	put(t, r, ".rsync_toolignore", "private/\n")
	put(t, r, "private/a", "secret")
	outside := t.TempDir()
	put(t, outside, "target", "secret")
	if err := os.Symlink(outside, filepath.Join(r, "link")); err != nil {
		t.Fatal(err)
	}
	cycle(t, e)
	if get(t, l, "sub/keep.log") != "yes" || len(get(t, l, "exact")) != int(DefaultMax) {
		t.Fatal("eligible missing")
	}
	for _, p := range []string{".git", "sub/drop.log", "sub/cache", "big", "private", "link"} {
		if _, err := os.Lstat(filepath.Join(l, p)); !os.IsNotExist(err) {
			t.Fatalf("excluded %s copied", p)
		}
	}
}
func TestIgnoreChangeDoesNotDeleteRemote(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "a", "keep")
	cycle(t, e)
	put(t, l, ".gitignore", "a\n")
	cycle(t, e)
	cycle(t, e)
	if get(t, r, "a") != "keep" {
		t.Fatal("ignored file deleted")
	}
}
func TestOversizeChangeDoesNotDelete(t *testing.T) {
	e, l, r := setup(t)
	put(t, r, "a", "small")
	cycle(t, e)
	put(t, l, "a", strings.Repeat("x", int(DefaultMax)+1))
	cycle(t, e)
	if get(t, r, "a") != "small" {
		t.Fatal("oversize treated as deletion")
	}
}
func TestConcurrentApplyAndPathSafety(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a", "old")
	old, _, _ := current(root, "a", DefaultMax)
	put(t, root, "a", "new")
	b := []byte("incoming")
	entry := &Entry{Hash: digest(b), Mode: 0644}
	if err := apply(root, DefaultMax, Change{File{Path: "a", Entry: entry, Data: b}, old, false}); err == nil {
		t.Fatal("concurrent write not rejected")
	}
	for _, p := range []string{"../escape", "/tmp/escape", ".git/config", "a/../escape"} {
		if _, err := safePath(root, p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "link"))
	if _, err := safePath(root, "link/file"); err == nil {
		t.Fatal("followed symlink")
	}
}
func TestRPCDeltaAndBatch(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a", "one")
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { defer b.Close(); done <- Serve(root, b, b) }()
	r, err := NewRPC(a, a, func() error { return a.Close() }, DefaultMax, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Scan()
	if err != nil || len(s.Files) != 1 {
		t.Fatalf("scan %v %v", s, err)
	}
	s, err = r.Scan()
	if err != nil || len(s.Files) != 1 {
		t.Fatal("delta lost unchanged file")
	}
	put(t, root, "a", "two")
	put(t, root, "b", "three")
	s, err = r.Scan()
	if err != nil || len(s.Files) != 2 {
		t.Fatal("delta missing changes")
	}
	files, err := r.Read([]string{"a", "b"})
	if err != nil || len(files) != 2 || !bytes.Equal(files[0].Data, []byte("two")) {
		t.Fatal("batch read")
	}
	os.Remove(filepath.Join(root, "a"))
	s, err = r.Scan()
	if err != nil || len(s.Files) != 1 {
		t.Fatal("delta missing deletion")
	}
	r.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
