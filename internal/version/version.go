// Package version holds the build version, set at link time:
//
//	go build -ldflags "-X github.com/boostrun/boostrun-cloud-controller-manager/internal/version.Version=v1.2.3"
package version

// Version is the release version, or "dev" for local builds.
var Version = "dev"

// UserAgent identifies this controller to the platform API.
func UserAgent() string { return "boostrun-cloud-controller-manager/" + Version }
