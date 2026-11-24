package collector

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/pprof/profile"
)

// ProfileKind identifies the pprof profile dimension to collect or analyze.
type ProfileKind string

const (
	// ProfileGoroutine represents the goroutine stack profile.
	ProfileGoroutine ProfileKind = "goroutine"
	// ProfileGoroutineLeak represents the Go 1.26+ GC-unreachable goroutine leak profile.
	ProfileGoroutineLeak ProfileKind = "goroutineleak"
	// ProfileHeap represents the heap allocation and inuse memory profile.
	ProfileHeap ProfileKind = "heap"
	// ProfileCPU represents the CPU execution profile.
	ProfileCPU ProfileKind = "cpu"
	// ProfileMutex represents the mutex contention profile.
	ProfileMutex ProfileKind = "mutex"
	// ProfileBlock represents the goroutine blocking profile.
	ProfileBlock ProfileKind = "block"
)

// ParseProfileKind parses a profile kind string (case-insensitive).
// It accepts "profile" as an alias for ProfileCPU.
func ParseProfileKind(raw string) (ProfileKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "goroutine":
		return ProfileGoroutine, nil
	case "goroutineleak":
		return ProfileGoroutineLeak, nil
	case "heap":
		return ProfileHeap, nil
	case "cpu", "profile":
		return ProfileCPU, nil
	case "mutex":
		return ProfileMutex, nil
	case "block":
		return ProfileBlock, nil
	default:
		return "", fmt.Errorf("unsupported profile kind %q", raw)
	}
}

// Valid reports whether k is a known profile kind.
func (k ProfileKind) Valid() bool {
	switch k {
	case ProfileGoroutine, ProfileGoroutineLeak, ProfileHeap, ProfileCPU, ProfileMutex, ProfileBlock:
		return true
	default:
		return false
	}
}

// Endpoint returns the net/http/pprof path segment corresponding to k.
func (k ProfileKind) Endpoint() string {
	if k == ProfileCPU {
		return "profile"
	}
	return string(k)
}

// Snapshot holds a parsed pprof profile along with collection metadata.
type Snapshot struct {
	Kind        ProfileKind
	CollectedAt time.Time
	Profile     *profile.Profile
}
