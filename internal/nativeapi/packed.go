//go:build ((linux && !android) || (darwin && !ios)) && rustapi

// Package nativeapi carries the platform-native Rust API into release CLIs.
package nativeapi

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

//go:embed payload.gz
var payload []byte

func Prepare() (string, func(), error) {
	directory, err := os.MkdirTemp("", "ormos-api-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	fail := func(err error) (string, func(), error) { cleanup(); return "", nil, err }
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fail(err)
	}
	defer reader.Close()
	path := filepath.Join(directory, "ormos-api")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(err)
	}
	size, err := io.Copy(file, io.LimitReader(reader, 64<<20+1))
	if err == nil && size > 64<<20 {
		err = fmt.Errorf("native API payload exceeds its bound")
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Chmod(0700)
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fail(err)
	}
	return path, cleanup, nil
}
