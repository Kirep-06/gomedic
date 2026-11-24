package version

import "fmt"

// Build-time variables populated via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a formatted version banner.
func String() string {
	return fmt.Sprintf("gomedic %s (commit %s, built %s)", Version, Commit, Date)
}
