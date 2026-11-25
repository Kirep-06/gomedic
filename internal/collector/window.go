package collector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const defaultWindowDuration = 30 * time.Second

// Fetcher retrieves a single pprof snapshot for a given profile kind.
type Fetcher interface {
	Fetch(ctx context.Context, kind ProfileKind, opts FetchOptions) (*Snapshot, error)
}

// WindowOptions configures a multi-dimensional time-window collection.
type WindowOptions struct {
	// Duration is the total observation window spanning t0 -> t_mid -> t1.
	// Defaults to 30s when zero. Must not be negative.
	Duration time.Duration
	// ForceGC requests a runtime GC before each heap snapshot (?gc=1).
	ForceGC bool
	// SkipCPU disables CPU profiling during the observation window.
	SkipCPU bool
}

// PointSeries holds chronological snapshots for a single profile dimension.
type PointSeries struct {
	T0   *Snapshot
	TMid *Snapshot
	T1   *Snapshot
}

// Complete reports whether all three sampling points (t0, t_mid, t1) were collected.
func (s PointSeries) Complete() bool {
	return s.T0 != nil && s.TMid != nil && s.T1 != nil
}

// HasDelta reports whether both window endpoints (t0 and t1) were collected.
func (s PointSeries) HasDelta() bool {
	return s.T0 != nil && s.T1 != nil
}

// WindowResult contains all snapshots collected across a single diagnostic window.
type WindowResult struct {
	StartedAt time.Time
	EndedAt   time.Time
	Duration  time.Duration

	Goroutine     PointSeries
	Heap          PointSeries
	Mutex         PointSeries
	Block         PointSeries
	CPU           *Snapshot
	GoroutineLeak *Snapshot

	// Skipped records profile dimensions that were unavailable or degraded
	// during collection, keyed by ProfileKind.
	Skipped map[ProfileKind]string
}

// WindowCollector orchestrates multi-dimensional pprof sampling across a time window.
type WindowCollector struct {
	fetcher Fetcher
}

// NewWindowCollector creates a WindowCollector backed by fetcher.
func NewWindowCollector(fetcher Fetcher) (*WindowCollector, error) {
	if fetcher == nil {
		return nil, errors.New("profile fetcher is nil")
	}
	return &WindowCollector{fetcher: fetcher}, nil
}

type fetchResult struct {
	snap *Snapshot
	err  error
}

// Collect captures multi-dimensional pprof snapshots across a t0 -> t_mid -> t1 window.
//
// At t0 it captures goroutine, heap, mutex, and block profiles while starting a concurrent
// CPU profile spanning the window. At t_mid it captures goroutine and heap profiles, and at
// t1 it captures goroutine, heap, mutex, block, and optional goroutineleak profiles.
//
// If both core dimensions (goroutine and heap) fail at t0, Collect aborts immediately
// instead of waiting for the window to elapse. Optional or unsupported endpoints degrade
// gracefully and are recorded in WindowResult.Skipped.
func (c *WindowCollector) Collect(ctx context.Context, opts WindowOptions) (*WindowResult, error) {
	if opts.Duration < 0 {
		return nil, fmt.Errorf("invalid window duration %s", opts.Duration)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	duration := opts.Duration
	if duration == 0 {
		duration = defaultWindowDuration
	}

	runCtx, cancel := context.WithCancel(ctx)

	var (
		cpuWg  sync.WaitGroup
		cpuRes fetchResult
	)
	// LIFO defer order ensures runCtx is canceled before waiting for any in-flight CPU fetch.
	defer cpuWg.Wait()
	defer cancel()

	startedAt := time.Now().UTC()
	res := &WindowResult{
		StartedAt: startedAt,
		Duration:  duration,
		Skipped:   make(map[ProfileKind]string),
	}

	if opts.SkipCPU {
		res.Skipped[ProfileCPU] = "disabled by options"
	} else {
		cpuSeconds := max(int(math.Round(duration.Seconds())), 1)
		cpuWg.Add(1)
		go func() {
			defer cpuWg.Done()
			snap, err := c.fetcher.Fetch(runCtx, ProfileCPU, FetchOptions{Seconds: cpuSeconds})
			cpuRes = fetchResult{snap: snap, err: err}
		}()
	}

	heapOpts := FetchOptions{GC: opts.ForceGC}

	// Phase t0: goroutine, heap, mutex, block.
	t0 := c.fetchBatch(runCtx, []batchRequest{
		{kind: ProfileGoroutine},
		{kind: ProfileHeap, opts: heapOpts},
		{kind: ProfileMutex},
		{kind: ProfileBlock},
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	gorT0 := t0[ProfileGoroutine]
	heapT0 := t0[ProfileHeap]
	if gorT0.err != nil && heapT0.err != nil {
		return nil, fmt.Errorf("failed to collect core profiles at t0: %w", errors.Join(gorT0.err, heapT0.err))
	}

	applySnapshot(&res.Goroutine.T0, ProfileGoroutine, "", gorT0, res.Skipped)
	applySnapshot(&res.Heap.T0, ProfileHeap, "", heapT0, res.Skipped)
	applySnapshot(&res.Mutex.T0, ProfileMutex, "", t0[ProfileMutex], res.Skipped)
	applySnapshot(&res.Block.T0, ProfileBlock, "", t0[ProfileBlock], res.Skipped)

	// Wait until t_mid.
	if err := sleepUntil(runCtx, startedAt.Add(duration/2)); err != nil {
		return nil, err
	}

	// Phase t_mid: goroutine and heap (only those that succeeded at t0).
	midReqs := make([]batchRequest, 0, 2)
	if res.Goroutine.T0 != nil {
		midReqs = append(midReqs, batchRequest{kind: ProfileGoroutine})
	}
	if res.Heap.T0 != nil {
		midReqs = append(midReqs, batchRequest{kind: ProfileHeap, opts: heapOpts})
	}
	tMid := c.fetchBatch(runCtx, midReqs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r, ok := tMid[ProfileGoroutine]; ok {
		applySnapshot(&res.Goroutine.TMid, ProfileGoroutine, "t_mid", r, res.Skipped)
	}
	if r, ok := tMid[ProfileHeap]; ok {
		applySnapshot(&res.Heap.TMid, ProfileHeap, "t_mid", r, res.Skipped)
	}

	// Wait until t1.
	if err := sleepUntil(runCtx, startedAt.Add(duration)); err != nil {
		return nil, err
	}

	// Phase t1: goroutine, heap, mutex, block, and optional goroutineleak.
	t1Reqs := make([]batchRequest, 0, 5)
	if res.Goroutine.T0 != nil {
		t1Reqs = append(t1Reqs, batchRequest{kind: ProfileGoroutine})
	}
	if res.Heap.T0 != nil {
		t1Reqs = append(t1Reqs, batchRequest{kind: ProfileHeap, opts: heapOpts})
	}
	if res.Mutex.T0 != nil {
		t1Reqs = append(t1Reqs, batchRequest{kind: ProfileMutex})
	}
	if res.Block.T0 != nil {
		t1Reqs = append(t1Reqs, batchRequest{kind: ProfileBlock})
	}
	t1Reqs = append(t1Reqs, batchRequest{kind: ProfileGoroutineLeak})

	t1 := c.fetchBatch(runCtx, t1Reqs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r, ok := t1[ProfileGoroutine]; ok {
		applySnapshot(&res.Goroutine.T1, ProfileGoroutine, "t1", r, res.Skipped)
	}
	if r, ok := t1[ProfileHeap]; ok {
		applySnapshot(&res.Heap.T1, ProfileHeap, "t1", r, res.Skipped)
	}
	if r, ok := t1[ProfileMutex]; ok {
		applySnapshot(&res.Mutex.T1, ProfileMutex, "t1", r, res.Skipped)
	}
	if r, ok := t1[ProfileBlock]; ok {
		applySnapshot(&res.Block.T1, ProfileBlock, "t1", r, res.Skipped)
	}
	if r, ok := t1[ProfileGoroutineLeak]; ok {
		applySnapshot(&res.GoroutineLeak, ProfileGoroutineLeak, "", r, res.Skipped)
	}

	cpuWg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !opts.SkipCPU {
		applySnapshot(&res.CPU, ProfileCPU, "", cpuRes, res.Skipped)
	}

	res.EndedAt = time.Now().UTC()
	return res, nil
}

type batchRequest struct {
	kind ProfileKind
	opts FetchOptions
}

func (c *WindowCollector) fetchBatch(ctx context.Context, reqs []batchRequest) map[ProfileKind]fetchResult {
	results := make([]fetchResult, len(reqs))
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(idx int, br batchRequest) {
			defer wg.Done()
			snap, err := c.fetcher.Fetch(ctx, br.kind, br.opts)
			results[idx] = fetchResult{snap: snap, err: err}
		}(i, req)
	}
	wg.Wait()

	out := make(map[ProfileKind]fetchResult, len(reqs))
	for i, req := range reqs {
		out[req.kind] = results[i]
	}
	return out
}

func applySnapshot(dst **Snapshot, kind ProfileKind, phase string, r fetchResult, skipped map[ProfileKind]string) {
	if r.err != nil {
		if _, exists := skipped[kind]; !exists {
			reason := formatSkipReason(kind, r.err)
			if phase != "" {
				reason = phase + ": " + reason
			}
			skipped[kind] = reason
		}
		return
	}
	*dst = r.snap
}

func formatSkipReason(kind ProfileKind, err error) string {
	if errors.Is(err, ErrProfileUnsupported) {
		if kind == ProfileGoroutineLeak {
			return "endpoint not supported by target (requires Go 1.26+)"
		}
		return "endpoint not supported by target (HTTP 404)"
	}
	return err.Error()
}

func sleepUntil(ctx context.Context, target time.Time) error {
	wait := time.Until(target)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
