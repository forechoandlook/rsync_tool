//go:build !helper

package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"rsync_tool/assets"
)

func helperData(target string) ([]byte, error) {
	b, e := assets.Helpers.ReadFile("payload/" + target + ".gz")
	if e != nil {
		return nil, fmt.Errorf("no embedded helper for %s; build using scripts/build.sh or use --remote-helper", target)
	}
	r, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	defer r.Close()
	return io.ReadAll(r)
}
