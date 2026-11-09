package syncer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func osRoot(root string) (string, error) {
	r, e := filepath.Abs(root)
	if e != nil {
		return "", e
	}
	r, e = filepath.EvalSymlinks(r)
	if e != nil {
		return "", e
	}
	st, e := os.Stat(r)
	if e == nil && !st.IsDir() {
		e = fmt.Errorf("not a directory: %s", root)
	}
	return r, e
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func ssh(host string, command string) *exec.Cmd {
	return exec.Command("ssh", "-C", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", host, command)
}

// Connect uploads an architecture-specific standalone helper, then opens a persistent compressed channel.
type Options struct {
	Watch               bool
	IgnoreRootGitignore bool
}

func Connect(spec string, max int64, extra []string, helper func(string) ([]byte, error), remoteHelper string, options ...Options) (Endpoint, error) {
	if strings.HasPrefix(spec, "local:") {
		root, e := osRoot(strings.TrimPrefix(spec, "local:"))
		if e != nil {
			return nil, e
		}
		s := NewScanner(root, max, extra)
		if len(options) > 0 {
			s.IgnoreRootGitignore = options[0].IgnoreRootGitignore
			if options[0].Watch {
				s.EnableWatch()
			}
		}
		return &Disk{s}, nil
	}
	i := strings.Index(spec, ":")
	if i < 1 {
		return nil, fmt.Errorf("remote must be [user@]host:/absolute/path")
	}
	host, root := spec[:i], spec[i+1:]
	if strings.HasPrefix(host, "-") || !strings.HasPrefix(root, "/") {
		return nil, fmt.Errorf("remote requires a host and absolute path")
	}
	cleanup := ""
	if remoteHelper == "" {
		cmd := ssh(host, "uname -s; uname -m")
		cmd.Stderr = os.Stderr
		b, e := cmd.Output()
		if e != nil {
			return nil, e
		}
		v := strings.Fields(string(b))
		if len(v) != 2 {
			return nil, fmt.Errorf("unexpected uname response: %q", b)
		}
		osname := strings.ToLower(v[0])
		arch := v[1]
		switch arch {
		case "x86_64":
			arch = "amd64"
		case "aarch64", "arm64":
			arch = "arm64"
		}
		data, e := helper(osname + "-" + arch)
		if e != nil {
			return nil, e
		}
		cmd = ssh(host, "umask 077; p=$(mktemp /tmp/rsync_tool.XXXXXXXX) || exit 1; if cat > \"$p\" && chmod 700 \"$p\"; then printf '%s' \"$p\"; else rm -f \"$p\"; exit 1; fi")
		cmd.Stdin = strings.NewReader(string(data))
		cmd.Stderr = os.Stderr
		b, e = cmd.Output()
		if e != nil {
			return nil, e
		}
		remoteHelper = strings.TrimSpace(string(b))
		if !strings.HasPrefix(remoteHelper, "/tmp/rsync_tool.") || strings.ContainsAny(remoteHelper, "\n\r") {
			return nil, fmt.Errorf("invalid helper path")
		}
		cleanup = "trap " + quote("rm -f -- "+quote(remoteHelper)) + " EXIT HUP INT TERM; "
	}
	cmd := ssh(host, cleanup+quote(remoteHelper)+" __serve "+quote(root))
	cmd.Stderr = os.Stderr
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	r, e := NewRPC(out, in, func() error { in.Close(); return cmd.Wait() }, max, extra, options...)
	if e != nil {
		in.Close()
		cmd.Wait()
		return nil, e
	}
	return r, nil
}
