// Package version carries the build-time version stamp.
package version

// Version is the release version, stamped at build time via
// -ldflags "-X .../internal/version.Version=x.y.z". "dev" otherwise.
var Version = "dev"
