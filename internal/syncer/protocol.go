package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type Request struct {
	Op                  string
	Max                 int64
	Extra               []string
	Paths               []string
	Changes             []Change
	Watch               bool
	IgnoreRootGitignore bool
}
type Response struct {
	Stats     ScanStats
	Unchanged bool
	Error     string
	Snapshot  Snapshot
	Removed   []string
	Files     []File
	Errors    map[string]string
}
type Endpoint interface {
	Scan() (Snapshot, error)
	Read([]string) ([]File, error)
	Apply([]Change) (map[string]string, error)
	Close() error
}
type Disk struct{ Scanner *Scanner }

func (d *Disk) Scan() (Snapshot, error) { return d.Scanner.Scan() }
func (d *Disk) Read(paths []string) ([]File, error) {
	out := []File{}
	for _, p := range paths {
		e, b, err := current(d.Scanner.Root, p, d.Scanner.Max)
		if err != nil {
			out = append(out, File{Path: p, Error: err.Error()})
			continue
		}
		out = append(out, File{Path: p, Entry: e, Data: b})
	}
	return out, nil
}
func (d *Disk) Apply(cs []Change) (map[string]string, error) {
	errs := map[string]string{}
	for _, c := range cs {
		if e := apply(d.Scanner.Root, d.Scanner.Max, c); e != nil {
			errs[c.File.Path] = e.Error()
		}
	}
	return errs, nil
}
func (d *Disk) Close() error { return d.Scanner.Close() }
func Serve(root string, in io.Reader, out io.Writer) error {
	root, e := osRoot(root)
	if e != nil {
		return e
	}
	d := &Disk{NewScanner(root, DefaultMax, nil)}
	defer d.Close()
	dec, enc := json.NewDecoder(in), json.NewEncoder(out)
	previous := map[string]Entry{}
	var revision uint64
	for {
		var q Request
		if e := dec.Decode(&q); e != nil {
			if e == io.EOF {
				return nil
			}
			return e
		}
		r := Response{}
		var err error
		switch q.Op {
		case "init":
			if q.Max <= 0 {
				err = fmt.Errorf("invalid max size")
			} else {
				d.Scanner.Max = q.Max
				d.Scanner.Extra = q.Extra
				d.Scanner.IgnoreRootGitignore = q.IgnoreRootGitignore
				if q.Watch {
					d.Scanner.EnableWatch()
				}
			}
		case "force":
			d.Scanner.ForceScan()
		case "scan":
			r.Snapshot, err = d.Scan()
			if err == nil && r.Snapshot.Revision == revision {
				r.Unchanged = true
				r.Snapshot = Snapshot{Stats: r.Snapshot.Stats}
			} else if err == nil {
				revision = r.Snapshot.Revision
				full := r.Snapshot.Files
				delta := map[string]Entry{}
				for p, v := range full {
					old, ok := previous[p]
					if !ok || old != v {
						delta[p] = v
					}
				}
				for p := range previous {
					if _, ok := full[p]; !ok {
						r.Removed = append(r.Removed, p)
					}
				}
				previous = full
				r.Snapshot.Files = delta
			}
		case "read":
			r.Files, err = d.Read(q.Paths)
		case "apply":
			r.Errors, err = d.Apply(q.Changes)
		default:
			err = fmt.Errorf("unknown operation %q", q.Op)
		}
		if err != nil {
			r.Error = err.Error()
		}
		var encodeErr error
		if r.Unchanged {
			encodeErr = enc.Encode(struct {
				Unchanged bool
				Stats     ScanStats
			}{true, r.Snapshot.Stats})
		} else {
			encodeErr = enc.Encode(r)
		}
		if e := encodeErr; e != nil {
			return e
		}
	}
}

type RPC struct {
	dec      *json.Decoder
	enc      *json.Encoder
	closer   func() error
	files    map[string]Entry
	snapshot Snapshot
}

func NewRPC(in io.Reader, out io.Writer, close func() error, max int64, extra []string, options ...Options) (*RPC, error) {
	r := &RPC{dec: json.NewDecoder(in), enc: json.NewEncoder(out), closer: close, files: map[string]Entry{}}
	q := Request{Op: "init", Max: max, Extra: extra}
	if len(options) > 0 {
		q.Watch = options[0].Watch
		q.IgnoreRootGitignore = options[0].IgnoreRootGitignore
	}
	_, e := r.call(q)
	return r, e
}
func (r *RPC) call(q Request) (Response, error) {
	var s Response
	if e := r.enc.Encode(q); e != nil {
		return s, &TransportError{e}
	}
	if e := r.dec.Decode(&s); e != nil {
		return s, &TransportError{e}
	}
	if s.Error != "" {
		return s, fmt.Errorf("remote: %s", s.Error)
	}
	return s, nil
}
func (r *RPC) Scan() (Snapshot, error) {
	s, e := r.call(Request{Op: "scan"})
	if e != nil {
		return Snapshot{}, e
	}
	if s.Unchanged {
		r.snapshot.Stats = s.Stats
		return r.snapshot, nil
	}
	for _, p := range s.Removed {
		delete(r.files, p)
	}
	for p, v := range s.Snapshot.Files {
		r.files[p] = v
	}
	s.Snapshot.Files = r.files
	r.snapshot = s.Snapshot
	return s.Snapshot, nil
}
func (r *RPC) Read(ps []string) ([]File, error) {
	s, e := r.call(Request{Op: "read", Paths: ps})
	return s.Files, e
}
func (r *RPC) Apply(cs []Change) (map[string]string, error) {
	s, e := r.call(Request{Op: "apply", Changes: cs})
	return s.Errors, e
}
func (r *RPC) Close() error { return r.closer() }

type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }
func IsTransportError(err error) bool   { var e *TransportError; return errors.As(err, &e) }
func (r *RPC) ForceScan() error         { _, err := r.call(Request{Op: "force"}); return err }
