package collector

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/pprof/profile"
)

// Parse decodes a pprof protobuf stream (gzip-compressed or uncompressed).
func Parse(r io.Reader) (*profile.Profile, error) {
	if r == nil {
		return nil, errors.New("profile reader is nil")
	}

	p, err := profile.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse pprof profile: %w", err)
	}
	return p, nil
}

// LoadFile reads and parses a local pprof profile file (.pb.gz or raw protobuf).
func LoadFile(path string) (*profile.Profile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("profile file path is empty")
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open profile file %q: %w", path, err)
	}
	defer func() {
		_ = f.Close()
	}()

	p, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("load profile file %q: %w", path, err)
	}
	return p, nil
}

// LoadSnapshot reads a local pprof file and wraps it in a Snapshot.
// If the profile embeds a capture timestamp, it is preserved; otherwise the current UTC time is used.
func LoadSnapshot(path string, kind ProfileKind) (*Snapshot, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("unsupported profile kind %q", kind)
	}

	p, err := LoadFile(path)
	if err != nil {
		return nil, err
	}

	collectedAt := time.Now().UTC()
	if p.TimeNanos > 0 {
		collectedAt = time.Unix(0, p.TimeNanos).UTC()
	}

	return &Snapshot{
		Kind:        kind,
		CollectedAt: collectedAt,
		Profile:     p,
	}, nil
}
