package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/pprof/profile"
)

func TestIsStdlibOrRuntime(t *testing.T) {
	tests := []struct {
		fn   string
		want bool
	}{
		{"runtime.gopark", true},
		{"runtime/internal/atomic.(*Uint32).Load", true},
		{"net/http.(*persistConn).readLoop", true},
		{"internal/poll.(*FD).Read", true},
		{"sync.(*Mutex).Lock", true},
		{"maps.Clone[go.shape.map[string]int,go.shape.string,go.shape.int]", true},
		{"_cgo_topofstack", true},
		{"", true},
		{"main.handleRequest", false},
		{"github.com/myorg/myapp/worker.(*Pool).Run", false},
		{"github.com/myorg/myapp/pool.(*Queue[github.com/myorg/myapp/model.Item]).Pop", false},
		{"gopkg.in/yaml.v3.(*Decoder).Decode", false},
		{"gopkg.in/yaml%2ev3.(*Decoder).Decode", false},
		{"example%2ecom.Handle", false},
		{"golang.org/x/sync/errgroup.(*Group).Go.func1", false},
	}

	for _, tc := range tests {
		if got := IsStdlibOrRuntime(tc.fn); got != tc.want {
			t.Errorf("IsStdlibOrRuntime(%q) = %v, want %v", tc.fn, got, tc.want)
		}
	}
}

func TestDetectModulePath(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "cmd", "server")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	// No go.mod in isolated temp dir (stop check only if parent has none; create explicit module below).
	modContent := "// header comment\nmodule \"github.com/myorg/myapp\" // trailing\n\ngo 1.26.0\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(modContent), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	got, err := DetectModulePath(nested)
	if err != nil {
		t.Fatalf("DetectModulePath(nested) error: %v", err)
	}
	if got != "github.com/myorg/myapp" {
		t.Fatalf("DetectModulePath(nested) = %q, want %q", got, "github.com/myorg/myapp")
	}

	// Passing a file path inside the module also resolves the module.
	fileInMod := filepath.Join(nested, "main.go")
	if err := os.WriteFile(fileInMod, []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if got, err := DetectModulePath(fileInMod); err != nil || got != "github.com/myorg/myapp" {
		t.Fatalf("DetectModulePath(file) = (%q, %v), want (%q, nil)", got, err, "github.com/myorg/myapp")
	}

	if _, err := DetectModulePath(filepath.Join(root, "does-not-exist")); err == nil {
		t.Fatal("DetectModulePath(nonexistent): expected error")
	}

	emptyModDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyModDir, "go.mod"), []byte("// empty\n"), 0o600); err != nil {
		t.Fatalf("write empty go.mod: %v", err)
	}
	if got, err := DetectModulePath(emptyModDir); err != nil || got != "" {
		t.Fatalf("DetectModulePath(empty go.mod) = (%q, %v), want (\"\", nil)", got, err)
	}

	badModDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badModDir, "go.mod"), []byte("module \"unclosed\n"), 0o600); err != nil {
		t.Fatalf("write bad go.mod: %v", err)
	}
	if _, err := DetectModulePath(badModDir); err == nil {
		t.Fatal("DetectModulePath(malformed quote): expected error")
	}
}

func TestAttributorPriorityAndSignature(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module github.com/myorg/myapp\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	mixedStack := []Frame{
		{Function: "runtime.gopark", File: "runtime/proc.go", Line: 400},
		{Function: "net/http.(*persistConn).roundTrip", File: "net/http/transport.go", Line: 2600},
		{Function: "github.com/myorg/myapp2/ext.Call", File: "ext.go", Line: 12},
		{Function: "github.com/myorg/myapp.v2/ext.Call", File: "ext2.go", Line: 14},
		{Function: "github.com/myorg/myapp/worker.(*Pool).consume", File: "worker/pool.go", Line: 88},
		{Function: "net/http.HandlerFunc.ServeHTTP", File: "net/http/server.go", Line: 2100},
		{Function: "github.com/myorg/myapp/worker.(*Pool).Run.func1", File: "worker/pool.go", Line: 42},
		{Function: "runtime.goexit", File: "runtime/asm.s", Line: 1693},
	}

	nonDomainStack := []Frame{
		{Function: "runtime.gopark", File: "runtime/proc.go", Line: 400},
		{Function: "myapp/worker.consume", File: "worker/worker.go", Line: 20},
		{Function: "myapp/worker.Run", File: "worker/worker.go", Line: 10},
		{Function: "runtime.goexit", File: "runtime/asm.s", Line: 1693},
	}

	escapedStack := []Frame{
		{Function: "runtime.gopark", File: "runtime/proc.go", Line: 400},
		{Function: "gopkg.in/yaml%2ev3.(*Decoder).Decode", File: "yaml.go", Line: 99},
	}

	stdlibOnlyStack := []Frame{
		{Function: "runtime.gopark", File: "runtime/proc.go", Line: 400},
		{Function: "net/http.(*persistConn).readLoop", File: "net/http/transport.go", Line: 2200},
		{Function: "runtime.goexit", File: "runtime/asm.s", Line: 1693},
	}

	tests := []struct {
		name        string
		opts        AttributionOptions
		frames      []Frame
		wantPrimary string
		wantSource  AttributionSource
		wantSig     string
	}{
		{
			name:        "explicit prefix beats go.mod and respects boundary",
			opts:        AttributionOptions{Packages: []string{"github.com/myorg/myapp/"}, WorkDir: modDir},
			frames:      mixedStack,
			wantPrimary: "github.com/myorg/myapp/worker.(*Pool).consume",
			wantSource:  SourceExplicit,
			wantSig:     "github.com/myorg/myapp/worker.(*Pool).consume;github.com/myorg/myapp/worker.(*Pool).Run.func1",
		},
		{
			name:        "auto-detected go.mod module",
			opts:        AttributionOptions{WorkDir: modDir},
			frames:      mixedStack,
			wantPrimary: "github.com/myorg/myapp/worker.(*Pool).consume",
			wantSource:  SourceModule,
			wantSig:     "github.com/myorg/myapp/worker.(*Pool).consume;github.com/myorg/myapp/worker.(*Pool).Run.func1",
		},
		{
			name:        "fallback strips stdlib and picks top non-stdlib frame",
			opts:        AttributionOptions{DisableGoMod: true},
			frames:      mixedStack,
			wantPrimary: "github.com/myorg/myapp2/ext.Call",
			wantSource:  SourceFallback,
			wantSig:     "github.com/myorg/myapp2/ext.Call;github.com/myorg/myapp.v2/ext.Call;github.com/myorg/myapp/worker.(*Pool).consume;github.com/myorg/myapp/worker.(*Pool).Run.func1",
		},
		{
			name:        "non-domain prefix retains business callers in signature",
			opts:        AttributionOptions{Packages: []string{"myapp"}, DisableGoMod: true},
			frames:      nonDomainStack,
			wantPrimary: "myapp/worker.consume",
			wantSource:  SourceExplicit,
			wantSig:     "myapp/worker.consume;myapp/worker.Run",
		},
		{
			name:        "percent-escaped versioned package matches explicit prefix",
			opts:        AttributionOptions{Packages: []string{"gopkg.in/yaml.v3"}, DisableGoMod: true},
			frames:      escapedStack,
			wantPrimary: "gopkg.in/yaml%2ev3.(*Decoder).Decode",
			wantSource:  SourceExplicit,
			wantSig:     "gopkg.in/yaml%2ev3.(*Decoder).Decode",
		},
		{
			name:        "stdlib only falls back to top frame",
			opts:        AttributionOptions{Packages: []string{"github.com/myorg/myapp"}},
			frames:      stdlibOnlyStack,
			wantPrimary: "runtime.gopark",
			wantSource:  SourceNone,
			wantSig:     "runtime.gopark;net/http.(*persistConn).readLoop",
		},
		{
			name:        "empty frames",
			opts:        AttributionOptions{DisableGoMod: true},
			frames:      nil,
			wantPrimary: "",
			wantSource:  SourceNone,
			wantSig:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attr, err := NewAttributor(tc.opts)
			if err != nil {
				t.Fatalf("NewAttributor() error: %v", err)
			}
			got := attr.AttributeFrames(tc.frames)
			if got.Primary.Function != tc.wantPrimary || got.Source != tc.wantSource || got.Signature != tc.wantSig {
				t.Fatalf("AttributeFrames() = (primary=%q, source=%q, sig=%q), want (%q, %q, %q)",
					got.Primary.Function, got.Source, got.Signature, tc.wantPrimary, tc.wantSource, tc.wantSig)
			}
		})
	}

	// Verify ModulePath accessor and nil receiver safety.
	modAttr, err := NewAttributor(AttributionOptions{WorkDir: modDir})
	if err != nil || modAttr.ModulePath() != "github.com/myorg/myapp" {
		t.Fatalf("ModulePath() = (%q, %v), want (%q, nil)", modAttr.ModulePath(), err, "github.com/myorg/myapp")
	}
	var nilAttr *Attributor
	if nilAttr.ModulePath() != "" {
		t.Fatalf("nil Attributor.ModulePath() = %q, want empty", nilAttr.ModulePath())
	}
	if got := nilAttr.AttributeFrames(mixedStack); got.Source != SourceFallback || got.Primary.Function != "github.com/myorg/myapp2/ext.Call" {
		t.Fatalf("nil Attributor.AttributeFrames() = (%q, %q), want fallback", got.Primary.Function, got.Source)
	}
}

func TestAttributeSampleWithInlinedFrames(t *testing.T) {
	attr, err := NewAttributor(AttributionOptions{
		Packages:     []string{"github.com/myorg/myapp"},
		DisableGoMod: true,
	})
	if err != nil {
		t.Fatalf("NewAttributor() error: %v", err)
	}

	if got := attr.AttributeSample(nil); got.Source != SourceNone || len(got.Frames) != 0 {
		t.Fatalf("AttributeSample(nil) = %+v, want empty SourceNone", got)
	}

	sample := &profile.Sample{
		Location: []*profile.Location{
			{
				ID: 1,
				Line: []profile.Line{
					{Function: &profile.Function{ID: 1, Name: "runtime.chanrecv1", Filename: "runtime/chan.go"}, Line: 442},
					{Function: &profile.Function{ID: 2, Name: "github.com/myorg/myapp/queue.(*Q[github.com/myorg/myapp/model.Job]).Pop", Filename: "queue/q.go"}, Line: 57},
				},
			},
			nil,
			{
				ID: 2,
				Line: []profile.Line{
					{Function: &profile.Function{ID: 3, Name: "main.workerLoop", Filename: "cmd/app/main.go"}, Line: 31},
				},
			},
		},
	}

	got := attr.AttributeSample(sample)
	wantPrimary := "github.com/myorg/myapp/queue.(*Q[github.com/myorg/myapp/model.Job]).Pop"
	if got.Source != SourceExplicit || got.Primary.Function != wantPrimary || got.Primary.Line != 57 {
		t.Fatalf("AttributeSample() = (primary=%+v, source=%q), want (%q:57, %q)",
			got.Primary, got.Source, wantPrimary, SourceExplicit)
	}
	if len(got.Frames) != 3 {
		t.Fatalf("len(Frames) = %d, want 3", len(got.Frames))
	}
	wantSig := wantPrimary + ";main.workerLoop"
	if got.Signature != wantSig {
		t.Fatalf("Signature = %q, want %q", got.Signature, wantSig)
	}
}
