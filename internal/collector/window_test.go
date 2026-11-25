package collector

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWindowCollectorCollect(t *testing.T) {
	var rawProfile bytes.Buffer
	if err := buildTestProfile(time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC).UnixNano()).Write(&rawProfile); err != nil {
		t.Fatalf("encode test profile: %v", err)
	}
	payload := rawProfile.Bytes()

	t.Run("full window with httptest", func(t *testing.T) {
		var (
			mu      sync.Mutex
			counts  = make(map[string]int)
			queries = make(map[string]string)
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			counts[r.URL.Path]++
			queries[r.URL.Path] = r.URL.RawQuery
			mu.Unlock()
			_, _ = w.Write(payload)
		}))
		defer srv.Close()

		client, err := NewHTTPClient(srv.URL, srv.Client())
		if err != nil {
			t.Fatalf("NewHTTPClient() error: %v", err)
		}
		wc, err := NewWindowCollector(client)
		if err != nil {
			t.Fatalf("NewWindowCollector() error: %v", err)
		}

		res, err := wc.Collect(context.Background(), WindowOptions{
			Duration: 40 * time.Millisecond,
			ForceGC:  true,
		})
		if err != nil {
			t.Fatalf("Collect() error: %v", err)
		}

		if !res.Goroutine.Complete() || !res.Heap.Complete() {
			t.Fatalf("expected complete goroutine/heap series, got goroutine=%+v heap=%+v", res.Goroutine, res.Heap)
		}
		if !res.Mutex.HasDelta() || !res.Block.HasDelta() {
			t.Fatalf("expected t0+t1 mutex/block series, got mutex=%+v block=%+v", res.Mutex, res.Block)
		}
		if res.CPU == nil || res.GoroutineLeak == nil {
			t.Fatalf("expected CPU and GoroutineLeak snapshots, got cpu=%v leak=%v", res.CPU != nil, res.GoroutineLeak != nil)
		}
		if len(res.Skipped) != 0 {
			t.Fatalf("expected no skipped profiles, got %v", res.Skipped)
		}
		if res.EndedAt.Before(res.StartedAt) {
			t.Fatalf("EndedAt %v before StartedAt %v", res.EndedAt, res.StartedAt)
		}

		wantCounts := map[string]int{
			"/debug/pprof/goroutine":     3,
			"/debug/pprof/heap":          3,
			"/debug/pprof/mutex":         2,
			"/debug/pprof/block":         2,
			"/debug/pprof/profile":       1,
			"/debug/pprof/goroutineleak": 1,
		}
		mu.Lock()
		defer mu.Unlock()
		for path, want := range wantCounts {
			if counts[path] != want {
				t.Errorf("request count for %s = %d, want %d", path, counts[path], want)
			}
		}
		if queries["/debug/pprof/heap"] != "gc=1" {
			t.Errorf("heap query = %q, want gc=1", queries["/debug/pprof/heap"])
		}
		if queries["/debug/pprof/profile"] != "seconds=1" {
			t.Errorf("cpu query = %q, want seconds=1", queries["/debug/pprof/profile"])
		}
	})

	t.Run("graceful degradation and skip cpu", func(t *testing.T) {
		var (
			mu        sync.Mutex
			heapCalls int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/debug/pprof/goroutine", "/debug/pprof/block":
				_, _ = w.Write(payload)
			case "/debug/pprof/heap":
				mu.Lock()
				heapCalls++
				mu.Unlock()
				http.Error(w, "heap disabled", http.StatusNotFound)
			case "/debug/pprof/goroutineleak", "/debug/pprof/mutex":
				http.Error(w, "Unknown profile", http.StatusNotFound)
			case "/debug/pprof/profile":
				http.Error(w, "cpu profiling already in use", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := NewHTTPClient(srv.URL, srv.Client())
		if err != nil {
			t.Fatalf("NewHTTPClient() error: %v", err)
		}
		wc, err := NewWindowCollector(client)
		if err != nil {
			t.Fatalf("NewWindowCollector() error: %v", err)
		}

		res, err := wc.Collect(context.Background(), WindowOptions{Duration: 20 * time.Millisecond})
		if err != nil {
			t.Fatalf("Collect() error: %v", err)
		}
		if !res.Goroutine.Complete() || !res.Block.HasDelta() {
			t.Fatalf("available profiles should succeed: gor=%v block=%v",
				res.Goroutine.Complete(), res.Block.HasDelta())
		}
		if res.Heap.T0 != nil || res.CPU != nil || res.GoroutineLeak != nil || res.Mutex.T0 != nil {
			t.Fatalf("degraded profiles should be nil")
		}
		mu.Lock()
		gotHeapCalls := heapCalls
		mu.Unlock()
		if gotHeapCalls != 1 {
			t.Errorf("failed core profile at t0 should not be re-fetched at t_mid/t1, got %d calls", gotHeapCalls)
		}
		if !strings.Contains(res.Skipped[ProfileHeap], "HTTP 404") {
			t.Errorf("unexpected heap skip reason: %q", res.Skipped[ProfileHeap])
		}
		if !strings.Contains(res.Skipped[ProfileGoroutineLeak], "Go 1.26+") {
			t.Errorf("unexpected goroutineleak skip reason: %q", res.Skipped[ProfileGoroutineLeak])
		}
		if !strings.Contains(res.Skipped[ProfileMutex], "HTTP 404") {
			t.Errorf("unexpected mutex skip reason: %q", res.Skipped[ProfileMutex])
		}
		if !strings.Contains(res.Skipped[ProfileCPU], "cpu profiling already in use") {
			t.Errorf("unexpected cpu skip reason: %q", res.Skipped[ProfileCPU])
		}

		resNoCPU, err := wc.Collect(context.Background(), WindowOptions{
			Duration: 20 * time.Millisecond,
			SkipCPU:  true,
		})
		if err != nil {
			t.Fatalf("Collect(SkipCPU) error: %v", err)
		}
		if resNoCPU.Skipped[ProfileCPU] != "disabled by options" {
			t.Errorf("SkipCPU reason = %q, want disabled by options", resNoCPU.Skipped[ProfileCPU])
		}
	})
}

func TestWindowCollectorErrorsAndCancel(t *testing.T) {
	if _, err := NewWindowCollector(nil); err == nil {
		t.Fatal("NewWindowCollector(nil): expected error")
	}

	var rawProfile bytes.Buffer
	if err := buildTestProfile(0).Write(&rawProfile); err != nil {
		t.Fatalf("encode test profile: %v", err)
	}
	payload := rawProfile.Bytes()

	t.Run("validation and canceled context", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(payload)
		}))
		defer srv.Close()

		client, err := NewHTTPClient(srv.URL, srv.Client())
		if err != nil {
			t.Fatalf("NewHTTPClient() error: %v", err)
		}
		wc, err := NewWindowCollector(client)
		if err != nil {
			t.Fatalf("NewWindowCollector() error: %v", err)
		}

		if _, err := wc.Collect(context.Background(), WindowOptions{Duration: -time.Second}); err == nil {
			t.Fatal("expected error for negative duration")
		}

		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := wc.Collect(canceledCtx, WindowOptions{Duration: time.Second}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("fail fast when core profiles fail at t0", func(t *testing.T) {
		var startOnce sync.Once
		cpuStarted := make(chan struct{})
		cpuCanceled := make(chan struct{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/debug/pprof/profile" {
				startOnce.Do(func() { close(cpuStarted) })
				<-r.Context().Done()
				select {
				case cpuCanceled <- struct{}{}:
				default:
				}
				return
			}
			select {
			case <-cpuStarted:
			case <-r.Context().Done():
				return
			}
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		client, err := NewHTTPClient(srv.URL, srv.Client())
		if err != nil {
			t.Fatalf("NewHTTPClient() error: %v", err)
		}
		wc, err := NewWindowCollector(client)
		if err != nil {
			t.Fatalf("NewWindowCollector() error: %v", err)
		}

		start := time.Now()
		_, err = wc.Collect(context.Background(), WindowOptions{Duration: 10 * time.Second})
		if err == nil {
			t.Fatal("expected error when both goroutine and heap fail at t0")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Collect did not fail fast: elapsed %v", elapsed)
		}
		select {
		case <-cpuCanceled:
		case <-time.After(2 * time.Second):
			t.Fatal("background CPU fetch was not canceled on t0 fail-fast")
		}
	})

	t.Run("cancel during window wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/debug/pprof/profile" {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write(payload)
		}))
		defer srv.Close()

		client, err := NewHTTPClient(srv.URL, srv.Client())
		if err != nil {
			t.Fatalf("NewHTTPClient() error: %v", err)
		}
		wc, err := NewWindowCollector(client)
		if err != nil {
			t.Fatalf("NewWindowCollector() error: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()

		_, err = wc.Collect(ctx, WindowOptions{Duration: 10 * time.Second})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
	})
}
