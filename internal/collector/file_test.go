package collector

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

func buildTestProfile(timeNanos int64) *profile.Profile {
	fn := &profile.Function{
		ID:         1,
		Name:       "github.com/example/app/worker.Run",
		SystemName: "github.com/example/app/worker.Run",
		Filename:   "worker/run.go",
	}
	loc := &profile.Location{
		ID: 1,
		Line: []profile.Line{
			{Function: fn, Line: 42},
		},
	}
	return &profile.Profile{
		TimeNanos: timeNanos,
		SampleType: []*profile.ValueType{
			{Type: "goroutine", Unit: "count"},
		},
		Sample: []*profile.Sample{
			{
				Location: []*profile.Location{loc},
				Value:    []int64{12},
			},
		},
		Location: []*profile.Location{loc},
		Function: []*profile.Function{fn},
	}
}

func TestParseProfileKind(t *testing.T) {
	tests := []struct {
		input   string
		want    ProfileKind
		wantErr bool
	}{
		{input: "goroutine", want: ProfileGoroutine},
		{input: "GOROUTINELEAK", want: ProfileGoroutineLeak},
		{input: " heap ", want: ProfileHeap},
		{input: "cpu", want: ProfileCPU},
		{input: "profile", want: ProfileCPU},
		{input: "mutex", want: ProfileMutex},
		{input: "block", want: ProfileBlock},
		{input: "", wantErr: true},
		{input: "unknown", wantErr: true},
	}

	for _, tc := range tests {
		got, err := ParseProfileKind(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseProfileKind(%q): expected error, got %q", tc.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseProfileKind(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseProfileKind(%q) = %q, want %q", tc.input, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("ProfileKind(%q).Valid() = false, want true", got)
		}
	}
}

func TestProfileKindEndpoint(t *testing.T) {
	if got := ProfileCPU.Endpoint(); got != "profile" {
		t.Fatalf("ProfileCPU.Endpoint() = %q, want %q", got, "profile")
	}
	if got := ProfileHeap.Endpoint(); got != "heap" {
		t.Fatalf("ProfileHeap.Endpoint() = %q, want %q", got, "heap")
	}
}

func TestParse(t *testing.T) {
	src := buildTestProfile(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).UnixNano())

	t.Run("gzip compressed", func(t *testing.T) {
		var buf bytes.Buffer
		if err := src.Write(&buf); err != nil {
			t.Fatalf("write compressed profile: %v", err)
		}

		got, err := Parse(&buf)
		if err != nil {
			t.Fatalf("Parse() error: %v", err)
		}
		if len(got.Sample) != 1 || got.Sample[0].Value[0] != 12 {
			t.Fatalf("unexpected samples: %+v", got.Sample)
		}
		if got.Function[0].Name != "github.com/example/app/worker.Run" {
			t.Fatalf("unexpected function name: %q", got.Function[0].Name)
		}
	})

	t.Run("uncompressed protobuf", func(t *testing.T) {
		var buf bytes.Buffer
		if err := src.WriteUncompressed(&buf); err != nil {
			t.Fatalf("write uncompressed profile: %v", err)
		}

		got, err := Parse(&buf)
		if err != nil {
			t.Fatalf("Parse() error: %v", err)
		}
		if len(got.Sample) != 1 || got.Sample[0].Value[0] != 12 {
			t.Fatalf("unexpected samples: %+v", got.Sample)
		}
	})

	t.Run("nil reader", func(t *testing.T) {
		if _, err := Parse(nil); err == nil {
			t.Fatal("expected error for nil reader")
		}
	})

	t.Run("invalid payload", func(t *testing.T) {
		if _, err := Parse(bytes.NewBufferString("not-a-valid-pprof")); err == nil {
			t.Fatal("expected error for invalid payload")
		}
	})
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "goroutine.pb.gz")

	var buf bytes.Buffer
	if err := buildTestProfile(0).Write(&buf); err != nil {
		t.Fatalf("encode test profile: %v", err)
	}
	if err := os.WriteFile(validPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write test profile file: %v", err)
	}

	got, err := LoadFile(validPath)
	if err != nil {
		t.Fatalf("LoadFile() unexpected error: %v", err)
	}
	if len(got.Sample) != 1 || got.Sample[0].Value[0] != 12 {
		t.Fatalf("unexpected samples: %+v", got.Sample)
	}

	if _, err := LoadFile(""); err == nil {
		t.Fatal("expected error for empty path")
	}

	_, err = LoadFile(filepath.Join(dir, "missing.pb.gz"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got: %v", err)
	}

	corruptPath := filepath.Join(dir, "corrupt.pb.gz")
	if err := os.WriteFile(corruptPath, []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := LoadFile(corruptPath); err == nil {
		t.Fatal("expected error for corrupt profile file")
	}
}

func TestLoadSnapshot(t *testing.T) {
	dir := t.TempDir()
	ts := time.Date(2026, 11, 24, 12, 30, 0, 0, time.UTC)

	withTimePath := filepath.Join(dir, "with_time.pb.gz")
	var buf bytes.Buffer
	if err := buildTestProfile(ts.UnixNano()).Write(&buf); err != nil {
		t.Fatalf("encode profile: %v", err)
	}
	if err := os.WriteFile(withTimePath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	snap, err := LoadSnapshot(withTimePath, ProfileGoroutine)
	if err != nil {
		t.Fatalf("LoadSnapshot() unexpected error: %v", err)
	}
	if snap.Kind != ProfileGoroutine {
		t.Fatalf("Kind = %q, want %q", snap.Kind, ProfileGoroutine)
	}
	if !snap.CollectedAt.Equal(ts) {
		t.Fatalf("CollectedAt = %v, want %v", snap.CollectedAt, ts)
	}
	if snap.Profile == nil {
		t.Fatal("expected non-nil Profile")
	}

	noTimePath := filepath.Join(dir, "no_time.pb.gz")
	buf.Reset()
	if err := buildTestProfile(0).Write(&buf); err != nil {
		t.Fatalf("encode profile: %v", err)
	}
	if err := os.WriteFile(noTimePath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	before := time.Now().UTC().Add(-time.Second)
	snapNoTime, err := LoadSnapshot(noTimePath, ProfileHeap)
	if err != nil {
		t.Fatalf("LoadSnapshot() without timestamp error: %v", err)
	}
	if snapNoTime.CollectedAt.Before(before) {
		t.Fatalf("expected fallback timestamp >= %v, got %v", before, snapNoTime.CollectedAt)
	}

	if _, err := LoadSnapshot(withTimePath, ProfileKind("invalid")); err == nil {
		t.Fatal("expected error for invalid profile kind")
	}
}
