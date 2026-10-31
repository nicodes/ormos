//go:build ((linux && !android) || (darwin && !ios)) && !rustapi

package nativeapi

import (
	"fmt"
	"os"
	"path/filepath"
)

// Source-only Go builds can select a separately built native API. Release
// binaries embed the exact native artifact using the rustapi build tag.
func Prepare() (string, func(), error) {
	path := os.Getenv("ORMOS_API_BINARY")
	if path == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", nil, err
		}
		path = filepath.Join(filepath.Dir(executable), "ormos-api")
	}
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("ORMOS_API_BINARY must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", nil, fmt.Errorf("native Rust API is unavailable; use a release CLI, run mise run build, or set ORMOS_API_BINARY to the built api executable")
	}
	return path, func() {}, nil
}
