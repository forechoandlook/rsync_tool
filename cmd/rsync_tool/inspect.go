package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"rsync_tool/internal/syncer"
	"time"
)

func inspect(args []string) error {
	f := flag.NewFlagSet("inspect", flag.ContinueOnError)
	samples := f.Int("samples", 10, "scan samples")
	interval := f.Duration("interval", 250*time.Millisecond, "sample interval")
	ignoreRoot := f.Bool("ignore-root-gitignore", false, "skip only the root .gitignore")
	watch := f.Bool("watch", true, "use filesystem notifications")
	max := f.Int64("max-size", syncer.DefaultMax, "max file bytes")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 || *samples < 1 {
		return fmt.Errorf("usage: rsync_tool inspect [--samples 10] host:/path")
	}
	start := time.Now()
	remote, err := syncer.Connect(f.Arg(0), *max, nil, helperData, "", syncer.Options{Watch: *watch, IgnoreRootGitignore: *ignoreRoot})
	if err != nil {
		return err
	}
	defer remote.Close()
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(map[string]any{"connect_ms": float64(time.Since(start).Microseconds()) / 1000})
	for n := 0; n < *samples; n++ {
		start = time.Now()
		s, err := remote.Scan()
		if err != nil {
			return err
		}
		var bytes int64
		for _, v := range s.Files {
			bytes += v.Size
		}
		enc.Encode(map[string]any{"sample": n, "scan_ms": float64(time.Since(start).Microseconds()) / 1000, "entries": len(s.Files), "eligible_bytes": bytes, "ignore_files": len(s.Rules), "blocked": len(s.Blocked), "stats": s.Stats})
		if n+1 < *samples {
			time.Sleep(*interval)
		}
	}
	return nil
}
