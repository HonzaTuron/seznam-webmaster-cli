// Package buildinfo exposes version data set by the release build.
package buildinfo

import "runtime/debug"

// Set with -ldflags by GoReleaser.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Resolved returns the version, falling back to module data for `go install`.
func Resolved() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}
