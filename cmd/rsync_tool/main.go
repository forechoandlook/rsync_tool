package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rsync_tool/internal/syncer"
)

type rules []string

func (r *rules) String() string     { return strings.Join(*r, ",") }
func (r *rules) Set(s string) error { *r = append(*r, s); return nil }
func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "rsync_tool:", e)
		os.Exit(1)
	}
}
func usage() {
	fmt.Print(`rsync_tool: bidirectional code directory sync over SSH

  rsync_tool start [options] user@host:/absolute/path ./local
  rsync_tool sync  [options] user@host:/absolute/path ./local
  rsync_tool inspect [--samples 10] [--ignore-root-gitignore] user@host:/path
  rsync_tool status [./local]
  rsync_tool stop   [./local]

start runs in the foreground; Ctrl-C or stop requests a final sync.
sync performs one cycle. State and conflict copies live in .rsync_tool/.

Options:
  -f pull|push            Force one direction, saving overwritten files
  --interval 750ms       Batch/poll interval (start only)
  --max-size 2097152     Maximum eligible file size in bytes
  --exclude PATTERN     Additional ignore rule (repeatable)
  --ignore-root-gitignore Skip root .gitignore; keep nested ignore rules
  --remote-helper PATH  Use an already installed remote executable

Use local:/absolute/path as the remote for local testing.
`)
}
func run(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		usage()
		return nil
	}
	if args[0] == "__serve" {
		if len(args) != 2 {
			return fmt.Errorf("helper requires directory")
		}
		return syncer.Serve(args[1], os.Stdin, os.Stdout)
	}
	command := args[0]
	if command == "inspect" {
		return inspect(args[1:])
	}
	if command == "status" || command == "stop" {
		root := "."
		if len(args) > 2 {
			return fmt.Errorf("expected one local directory")
		}
		if len(args) == 2 {
			root = args[1]
		}
		abs, e := filepath.Abs(root)
		if e != nil {
			return e
		}
		if command == "stop" {
			return stop(abs)
		}
		s, e := syncer.Load(abs)
		if e != nil {
			return e
		}
		fmt.Printf("remote: %s\nrunning: %t\nlast sync: %s\ntracked files: %d\nconflicts: %d\n", s.Remote, isRunning(abs), s.LastSync.Format(time.RFC3339), len(s.Base), len(s.Conflicts))
		if s.LastError != "" {
			fmt.Println("last error:", s.LastError)
		}
		return nil
	}
	if command != "start" && command != "sync" {
		return fmt.Errorf("unknown command %q", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	force := fs.String("f", "", "force pull or push")
	interval := fs.Duration("interval", 750*time.Millisecond, "poll interval")
	ignoreRoot := fs.Bool("ignore-root-gitignore", false, "skip only the root .gitignore")
	max := fs.Int64("max-size", syncer.DefaultMax, "max file bytes")
	remoteHelper := fs.String("remote-helper", "", "installed remote helper")
	var extra rules
	fs.Var(&extra, "exclude", "ignore pattern")
	// Accept options before or after positional arguments.
	flags, pos := []string{}, []string{}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && a != "-h" && a != "--help" && a != "--ignore-root-gitignore" {
				i++
				if i >= len(args) {
					return fmt.Errorf("missing option value for %s", a)
				}
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	if e := fs.Parse(flags); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: rsync_tool %s [options] user@host:/absolute/path ./local", command)
	}
	if *force != "" && *force != "pull" && *force != "push" {
		return fmt.Errorf("-f must be pull or push")
	}
	if *max <= 0 || *interval < 50*time.Millisecond {
		return fmt.Errorf("max-size must be positive; interval must be at least 50ms")
	}
	root, e := filepath.Abs(pos[1])
	if e != nil {
		return e
	}
	if e = os.MkdirAll(root, 0755); e != nil {
		return e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	if strings.HasPrefix(pos[0], "local:") {
		other, _ := filepath.Abs(strings.TrimPrefix(pos[0], "local:"))
		other, _ = filepath.EvalSymlinks(other)
		if other == root || strings.HasPrefix(other, root+string(os.PathSeparator)) || strings.HasPrefix(root, other+string(os.PathSeparator)) {
			return fmt.Errorf("local test directories must not overlap")
		}
	}
	lock, e := acquire(root)
	if e != nil {
		return e
	}
	defer lock.Close()
	state, e := syncer.Load(root)
	if e != nil {
		return e
	}
	if state.Remote != "" && state.Remote != pos[0] {
		return fmt.Errorf("directory is already paired with %s; use a new local directory", state.Remote)
	}
	state.Remote = pos[0]
	state.PID = os.Getpid()
	state.Running = true
	if e = syncer.Save(root, state); e != nil {
		return e
	}
	defer func() {
		state.Running = false
		state.PID = 0
		if err := syncer.Save(root, state); err != nil {
			fmt.Fprintln(os.Stderr, "save state:", err)
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	log := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
	}
	connect := func() (syncer.Endpoint, error) {
		return syncer.Connect(pos[0], *max, extra, helperData, *remoteHelper, syncer.Options{Watch: command == "start", IgnoreRootGitignore: *ignoreRoot})
	}
	remote, e := connect()
	if e != nil {
		state.LastError = e.Error()
		return e
	}
	defer func() {
		if remote != nil {
			remote.Close()
		}
	}()
	engine := syncer.Engine{Local: &syncer.Disk{Scanner: syncer.NewScanner(root, *max, extra)}, Remote: remote, State: &state, Force: *force, Log: log}
	engine.Local.Scanner.IgnoreRootGitignore = *ignoreRoot
	if command == "start" {
		engine.Local.Scanner.EnableWatch()
	}
	defer engine.Local.Close()
	lastSave := time.Now()
	cycle := func() error {
		err := engine.Cycle()
		if err != nil {
			state.LastError = err.Error()
		}
		if engine.Changed || err != nil || time.Since(lastSave) >= 10*time.Second {
			if saveErr := syncer.Save(root, state); saveErr != nil {
				return saveErr
			}
			lastSave = time.Now()
		}
		return err
	}
	if command == "sync" {
		if e = cycle(); e != nil {
			return e
		}
		if len(state.Conflicts) > 0 {
			return fmt.Errorf("%d unresolved conflicts; copies saved in .rsync_tool/conflicts", len(state.Conflicts))
		}
		return nil
	}
	log("started: %s <-> %s", pos[0], root)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if remote == nil {
			remote, e = connect()
			if e == nil {
				engine.Remote = remote
			}
		}
		if remote != nil {
			e = cycle()
		}
		if e != nil {
			log("sync failed, retrying: %v", e)
			state.LastError = e.Error()
			syncer.Save(root, state)
			if remote != nil && syncer.IsTransportError(e) {
				remote.Close()
				remote = nil
			}
		}
		// Force applies only to the first successful cycle; subsequent changes are bidirectional.
		if e == nil {
			engine.Force = ""
		}
		select {
		case <-signals:
			log("stopping; flushing pending changes")
			if remote == nil {
				remote, e = connect()
				if e != nil {
					return e
				}
				engine.Remote = remote
			}
			engine.Local.Scanner.ForceScan()
			if r, ok := remote.(*syncer.RPC); ok {
				if err := r.ForceScan(); err != nil {
					return err
				}
			} else if d, ok := remote.(*syncer.Disk); ok {
				d.Scanner.ForceScan()
			}
			for attempt := 0; attempt < 3; attempt++ {
				e = cycle()
				if e == nil {
					break
				}
				if syncer.IsTransportError(e) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if e != nil {
				return fmt.Errorf("final sync failed: %w", e)
			}
			if len(state.Conflicts) > 0 {
				return fmt.Errorf("stopped with %d unresolved conflicts", len(state.Conflicts))
			}
			log("stopped; changes flushed")
			return nil
		case <-ticker.C:
		}
	}
}
func lockPath(root string) string { return filepath.Join(root, ".rsync_tool", "lock") }
func acquire(root string) (*os.File, error) {
	if e := os.MkdirAll(filepath.Dir(lockPath(root)), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(lockPath(root), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("sync already running for %s", root)
	}
	if e = f.Truncate(0); e != nil {
		f.Close()
		return nil, e
	}
	if _, e = fmt.Fprintf(f, "%d", os.Getpid()); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func isRunning(root string) bool {
	f, e := os.OpenFile(lockPath(root), os.O_RDWR, 0600)
	if e != nil {
		return false
	}
	defer f.Close()
	e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return e != nil
}
func stop(root string) error {
	if !isRunning(root) {
		return fmt.Errorf("no running sync for %s", root)
	}
	b, e := os.ReadFile(lockPath(root))
	if e != nil {
		return e
	}
	pid, e := strconv.Atoi(string(b))
	if e != nil || pid <= 1 {
		return fmt.Errorf("invalid sync pid")
	}
	if e = syscall.Kill(pid, syscall.SIGTERM); e != nil {
		return e
	}
	deadline := time.Now().Add(60 * time.Second)
	for isRunning(root) {
		if time.Now().After(deadline) {
			return fmt.Errorf("stop requested; still waiting for final sync")
		}
		time.Sleep(100 * time.Millisecond)
	}
	s, e := syncer.Load(root)
	if e != nil {
		return e
	}
	if s.LastError != "" {
		return fmt.Errorf("stopped with error: %s", s.LastError)
	}
	if len(s.Conflicts) > 0 {
		return fmt.Errorf("stopped with %d unresolved conflicts", len(s.Conflicts))
	}
	fmt.Println("stopped; changes flushed")
	return nil
}
