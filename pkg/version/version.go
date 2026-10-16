// Package version holds the build version, set at link time with
// -ldflags "-X github.com/hononeko/thread-udp-proxy/pkg/version.Version=<version>".
package version

// Version is the released version of this build, or "dev" for local and PR builds.
var Version = "dev"
