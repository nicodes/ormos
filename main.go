//go:build linux || darwin

// Command ormos runs the system on a personal machine.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/nicodes/ormos/internal/system"
)

// version is the build version, overridden at release time via
// -ldflags "-X main.version=...". Defaults to "dev" for local builds.
var version = "dev"

func main() { system.Main(os.Args[1:], resolvedVersion(version)) }

// resolvedVersion keeps release archives and module-aware `go run` builds
// consistent. Archives stamp version with -ldflags; `go run ...@vX.Y.Z`
// records it in the binary's Go build information.
func resolvedVersion(stamped string) string {
	if stamped != "" && stamped != "dev" {
		return strings.TrimPrefix(stamped, "v")
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		if v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}
