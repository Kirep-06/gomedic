package analyzer

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/pprof/profile"
)

// AttributionSource indicates which priority tier selected the primary frame.
type AttributionSource string

const (
	// SourceExplicit indicates a frame matched an explicit --pkg / config prefix.
	SourceExplicit AttributionSource = "explicit"
	// SourceModule indicates a frame matched the auto-detected go.mod module path.
	SourceModule AttributionSource = "module"
	// SourceFallback indicates a non-stdlib frame was selected by stripping stdlib/runtime frames.
	SourceFallback AttributionSource = "fallback"
	// SourceNone indicates no business or non-stdlib frame existed; fell back to the top frame.
	SourceNone AttributionSource = "none"
)

// Frame represents a single resolved stack frame from a pprof sample.
type Frame struct {
	Function string
	File     string
	Line     int64
}

// AttributedStack holds the resolved frames of a pprof sample along with its
// attributed business frame and stable correlation signature.
type AttributedStack struct {
	Primary   Frame
	Source    AttributionSource
	Frames    []Frame
	Signature string
}

// AttributionOptions configures how Attributor resolves business frames.
type AttributionOptions struct {
	// Packages lists explicit business package prefixes (highest priority).
	Packages []string
	// WorkDir is the starting directory for go.mod auto-detection when Packages is empty.
	// If empty, the current working directory is used.
	WorkDir string
	// DisableGoMod disables automatic go.mod lookup.
	DisableGoMod bool
}

// Attributor maps raw pprof call stacks to the most relevant business code frame.
type Attributor struct {
	explicitPrefixes []string
	modulePrefix     string
}

// NewAttributor creates an Attributor following the priority chain:
// explicit package prefixes -> auto-detected go.mod module path -> non-stdlib fallback.
func NewAttributor(opts AttributionOptions) (*Attributor, error) {
	explicit := normalizePrefixes(opts.Packages)

	var modPath string
	if len(explicit) == 0 && !opts.DisableGoMod {
		detected, err := DetectModulePath(opts.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("detect go.mod module path: %w", err)
		}
		modPath = normalizePrefix(detected)
	}

	return &Attributor{
		explicitPrefixes: explicit,
		modulePrefix:     modPath,
	}, nil
}

// ModulePath returns the auto-detected go.mod module path, if any.
func (a *Attributor) ModulePath() string {
	if a == nil {
		return ""
	}
	return a.modulePrefix
}

// AttributeSample resolves a pprof sample's locations into frames and attributes
// the stack to the closest relevant business frame from the top of the stack.
func (a *Attributor) AttributeSample(sample *profile.Sample) AttributedStack {
	if sample == nil {
		return AttributedStack{Source: SourceNone}
	}
	return a.AttributeFrames(ExtractFrames(sample))
}

// AttributeFrames attributes an already-resolved frame slice (ordered top-to-bottom).
func (a *Attributor) AttributeFrames(frames []Frame) AttributedStack {
	if len(frames) == 0 {
		return AttributedStack{Source: SourceNone}
	}

	primaryIdx := -1
	source := SourceNone

	if a != nil && len(a.explicitPrefixes) > 0 {
		for i, f := range frames {
			if matchesAnyPrefix(f.Function, a.explicitPrefixes) {
				primaryIdx = i
				source = SourceExplicit
				break
			}
		}
	}

	if primaryIdx == -1 && a != nil && a.modulePrefix != "" {
		for i, f := range frames {
			if matchesPrefix(f.Function, a.modulePrefix) {
				primaryIdx = i
				source = SourceModule
				break
			}
		}
	}

	if primaryIdx == -1 {
		for i, f := range frames {
			if !IsStdlibOrRuntime(f.Function) {
				primaryIdx = i
				source = SourceFallback
				break
			}
		}
	}

	if primaryIdx == -1 {
		primaryIdx = 0
		source = SourceNone
	}

	return AttributedStack{
		Primary:   frames[primaryIdx],
		Source:    source,
		Frames:    frames,
		Signature: a.buildSignature(frames, primaryIdx, source),
	}
}

// ExtractFrames expands a pprof sample's locations and inline lines into a
// flat frame slice ordered from top of stack (callee) to bottom (caller).
func ExtractFrames(sample *profile.Sample) []Frame {
	if sample == nil || len(sample.Location) == 0 {
		return nil
	}

	frames := make([]Frame, 0, len(sample.Location))
	for _, loc := range sample.Location {
		if loc == nil {
			continue
		}
		for _, line := range loc.Line {
			if line.Function == nil || line.Function.Name == "" {
				continue
			}
			frames = append(frames, Frame{
				Function: line.Function.Name,
				File:     line.Function.Filename,
				Line:     line.Line,
			})
		}
	}
	return frames
}

// DetectModulePath walks up from startDir (or the current working directory if empty)
// looking for a go.mod file and returns its declared module path.
// It returns ("", nil) when no go.mod file is found.
func DetectModulePath(startDir string) (string, error) {
	dir := strings.TrimSpace(startDir)
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		dir = wd
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path %q: %w", dir, err)
	}

	info, err := os.Stat(absDir)
	if err != nil {
		return "", fmt.Errorf("stat directory %q: %w", absDir, err)
	}
	if !info.IsDir() {
		absDir = filepath.Dir(absDir)
	}

	for {
		candidate := filepath.Join(absDir, "go.mod")
		fi, statErr := os.Stat(candidate)
		if statErr == nil && !fi.IsDir() {
			return parseGoModModule(candidate)
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			if errors.Is(statErr, os.ErrPermission) {
				return "", nil
			}
			return "", fmt.Errorf("stat %q: %w", candidate, statErr)
		}

		parent := filepath.Dir(absDir)
		if parent == absDir {
			return "", nil
		}
		absDir = parent
	}
}

func parseGoModModule(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			raw := fields[1]
			if strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, "`") {
				unquoted, err := strconv.Unquote(raw)
				if err != nil {
					return "", fmt.Errorf("parse module directive in %q: %w", path, err)
				}
				return strings.TrimSpace(unquoted), nil
			}
			return strings.TrimSpace(raw), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return "", nil
}

// IsStdlibOrRuntime reports whether a pprof symbol belongs to the Go standard library,
// Go runtime, or an unattributed system frame. The user's "main" package is not considered stdlib.
func IsStdlibOrRuntime(funcName string) bool {
	pkg := extractPackagePath(funcName)
	if pkg == "" {
		return true
	}
	if pkg == "main" {
		return false
	}

	firstSegment := pkg
	if slash := strings.IndexByte(pkg, '/'); slash >= 0 {
		firstSegment = pkg[:slash]
	}
	return !strings.ContainsRune(firstSegment, '.')
}

func extractPackagePath(funcName string) string {
	head := strings.TrimSpace(funcName)
	if head == "" {
		return ""
	}
	if bracket := strings.IndexByte(head, '['); bracket >= 0 {
		head = head[:bracket]
	}

	var rawPkg string
	if lastSlash := strings.LastIndexByte(head, '/'); lastSlash >= 0 {
		rest := head[lastSlash+1:]
		dot := strings.IndexByte(rest, '.')
		if dot <= 0 {
			return ""
		}
		rawPkg = head[:lastSlash+1+dot]
	} else {
		dot := strings.IndexByte(head, '.')
		if dot <= 0 {
			return ""
		}
		rawPkg = head[:dot]
	}

	return unescapePkgPath(rawPkg)
}

func unescapePkgPath(raw string) string {
	if !strings.ContainsRune(raw, '%') {
		return raw
	}
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

func normalizePrefixes(prefixes []string) []string {
	if len(prefixes) == 0 {
		return nil
	}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if norm := normalizePrefix(p); norm != "" {
			out = append(out, norm)
		}
	}
	return out
}

func normalizePrefix(p string) string {
	return strings.TrimRight(strings.TrimSpace(p), "/")
}

func matchesAnyPrefix(funcName string, prefixes []string) bool {
	for _, p := range prefixes {
		if matchesPrefix(funcName, p) {
			return true
		}
	}
	return false
}

// matchesPrefix checks whether funcName belongs to prefix or one of its subpackages
// while enforcing path/symbol boundaries (e.g. "foo/bar" does not match "foo/bar2"
// or "foo/bar.v2/pkg.Func").
func matchesPrefix(funcName, prefix string) bool {
	if prefix == "" || funcName == "" {
		return false
	}

	pkg := extractPackagePath(funcName)
	if pkg != "" && (pkg == prefix || (strings.HasPrefix(pkg, prefix) && pkg[len(prefix)] == '/')) {
		return true
	}

	// Fallback for unescaped dotted package names (e.g. "gopkg.in/yaml.v3.(*Decoder).Decode")
	// where extractPackagePath stops at the first literal dot in the last segment.
	head := funcName
	if bracket := strings.IndexByte(head, '['); bracket >= 0 {
		head = head[:bracket]
	}
	if !strings.HasPrefix(head, prefix) || len(head) <= len(prefix) {
		return false
	}
	next := head[len(prefix)]
	if next == '/' {
		return true
	}
	return next == '.' && !strings.ContainsRune(head[len(prefix)+1:], '/')
}

func (a *Attributor) matchesConfigured(funcName string) bool {
	if a == nil {
		return false
	}
	if matchesAnyPrefix(funcName, a.explicitPrefixes) {
		return true
	}
	return a.modulePrefix != "" && matchesPrefix(funcName, a.modulePrefix)
}

func (a *Attributor) buildSignature(frames []Frame, primaryIdx int, source AttributionSource) string {
	if len(frames) == 0 {
		return ""
	}

	parts := make([]string, 0, len(frames)-primaryIdx)
	if source == SourceNone {
		for _, f := range frames {
			if f.Function == "" || f.Function == "runtime.goexit" {
				continue
			}
			parts = append(parts, f.Function)
		}
	} else {
		for i := primaryIdx; i < len(frames); i++ {
			fn := frames[i].Function
			if fn == "" || fn == "runtime.goexit" {
				continue
			}
			if i == primaryIdx || a.matchesConfigured(fn) || !IsStdlibOrRuntime(fn) {
				parts = append(parts, fn)
			}
		}
	}

	if len(parts) == 0 {
		return frames[primaryIdx].Function
	}
	return strings.Join(parts, ";")
}
