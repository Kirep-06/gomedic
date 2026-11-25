package collector

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewHTTPClient(t *testing.T) {
	tests := []struct {
		rawURL  string
		wantURL string
		wantErr bool
	}{
		{rawURL: "http://127.0.0.1:6060", wantURL: "http://127.0.0.1:6060/debug/pprof/heap"},
		{rawURL: "http://127.0.0.1:6060/", wantURL: "http://127.0.0.1:6060/debug/pprof/heap"},
		{rawURL: "http://127.0.0.1:6060/debug/pprof/", wantURL: "http://127.0.0.1:6060/debug/pprof/heap"},
		{rawURL: "https://svc.local:8443/admin/debug/pprof", wantURL: "https://svc.local:8443/admin/debug/pprof/heap"},
		{rawURL: "  http://127.0.0.1:6060/internal?a=1#frag  ", wantURL: "http://127.0.0.1:6060/internal/debug/pprof/heap"},
		{rawURL: "", wantErr: true},
		{rawURL: "127.0.0.1:6060", wantErr: true},
		{rawURL: "ftp://127.0.0.1:6060", wantErr: true},
		{rawURL: "http:///debug/pprof", wantErr: true},
	}

	for _, tc := range tests {
		client, err := NewHTTPClient(tc.rawURL, nil)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NewHTTPClient(%q): expected error", tc.rawURL)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NewHTTPClient(%q): unexpected error: %v", tc.rawURL, err)
		}
		if got, err := client.EndpointURL(ProfileHeap, FetchOptions{}); err != nil || got != tc.wantURL {
			t.Fatalf("EndpointURL(%q) = (%q, %v), want (%q, nil)", tc.rawURL, got, err, tc.wantURL)
		}
	}
}

func TestHTTPClientFetch(t *testing.T) {
	ts := time.Date(2026, 11, 25, 5, 0, 0, 0, time.UTC)
	var withTime, withoutTime bytes.Buffer
	if err := buildTestProfile(ts.UnixNano()).Write(&withTime); err != nil {
		t.Fatalf("encode profile: %v", err)
	}
	if err := buildTestProfile(0).Write(&withoutTime); err != nil {
		t.Fatalf("encode profile without time: %v", err)
	}

	type reqInfo struct{ path, query, ua string }
	var (
		mu   sync.Mutex
		last reqInfo
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = reqInfo{path: r.URL.Path, query: r.URL.RawQuery, ua: r.Header.Get("User-Agent")}
		mu.Unlock()

		switch r.URL.Path {
		case "/debug/pprof/goroutine", "/debug/pprof/profile":
			_, _ = w.Write(withTime.Bytes())
		case "/debug/pprof/heap":
			_, _ = w.Write(withoutTime.Bytes())
		case "/debug/pprof/goroutineleak":
			http.Error(w, "Unknown profile", http.StatusNotFound)
		case "/debug/pprof/mutex":
			http.Error(w, strings.Repeat("x", maxErrorBodyBytes+100), http.StatusInternalServerError)
		case "/debug/pprof/block":
			_, _ = w.Write([]byte("<html>not pprof</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewHTTPClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}

	okCases := []struct {
		kind      ProfileKind
		opts      FetchOptions
		wantPath  string
		wantQuery string
		exactTS   bool
	}{
		{ProfileGoroutine, FetchOptions{}, "/debug/pprof/goroutine", "", true},
		{ProfileCPU, FetchOptions{Seconds: 5}, "/debug/pprof/profile", "seconds=5", true},
		{ProfileHeap, FetchOptions{GC: true}, "/debug/pprof/heap", "gc=1", false},
	}
	for _, tc := range okCases {
		before := time.Now().UTC().Add(-time.Second)
		snap, err := client.Fetch(context.Background(), tc.kind, tc.opts)
		if err != nil {
			t.Fatalf("Fetch(%s) error: %v", tc.kind, err)
		}
		mu.Lock()
		got := last
		mu.Unlock()
		if got.path != tc.wantPath || got.query != tc.wantQuery || got.ua != "gomedic" {
			t.Fatalf("Fetch(%s) request = %+v, want path=%q query=%q ua=gomedic", tc.kind, got, tc.wantPath, tc.wantQuery)
		}
		if tc.exactTS && !snap.CollectedAt.Equal(ts) {
			t.Fatalf("Fetch(%s) CollectedAt = %v, want %v", tc.kind, snap.CollectedAt, ts)
		}
		if !tc.exactTS && snap.CollectedAt.Before(before) {
			t.Fatalf("Fetch(%s) fallback timestamp %v < %v", tc.kind, snap.CollectedAt, before)
		}
	}

	t.Run("errors and degradation", func(t *testing.T) {
		var statusErr *HTTPStatusError

		_, err := client.Fetch(context.Background(), ProfileGoroutineLeak, FetchOptions{})
		if !errors.Is(err, ErrProfileUnsupported) || !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
			t.Fatalf("404 error = %v, want ErrProfileUnsupported + 404", err)
		}

		_, err = client.Fetch(context.Background(), ProfileMutex, FetchOptions{})
		if errors.Is(err, ErrProfileUnsupported) || !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("500 error = %v, want HTTPStatusError 500", err)
		}
		if len(statusErr.Body) > maxErrorBodyBytes {
			t.Fatalf("error body length %d > %d", len(statusErr.Body), maxErrorBodyBytes)
		}

		if _, err := client.Fetch(context.Background(), ProfileBlock, FetchOptions{}); err == nil {
			t.Fatal("expected decode error for non-pprof body")
		}
		if _, err := client.Fetch(context.Background(), ProfileKind("bad"), FetchOptions{}); err == nil {
			t.Fatal("expected error for invalid kind")
		}
		if _, err := client.Fetch(context.Background(), ProfileCPU, FetchOptions{Seconds: -1}); err == nil {
			t.Fatal("expected error for negative seconds")
		}
	})

	t.Run("concurrent fetches", func(t *testing.T) {
		var wg sync.WaitGroup
		errCh := make(chan error, 6)
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, fetchErr := client.Fetch(context.Background(), ProfileGoroutine, FetchOptions{})
				errCh <- fetchErr
			}()
		}
		wg.Wait()
		close(errCh)
		for fetchErr := range errCh {
			if fetchErr != nil {
				t.Fatalf("concurrent Fetch() error: %v", fetchErr)
			}
		}
	})
}

func TestHTTPClientRedactsCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	u.User = url.UserPassword("alice", "super-secret")

	client, err := NewHTTPClient(u.String(), srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}
	if _, err := client.Fetch(context.Background(), ProfileHeap, FetchOptions{}); err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("expected redacted error, got: %v", err)
	}
}

func TestHTTPClientContextCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := NewHTTPClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, fetchErr := client.Fetch(ctx, ProfileGoroutine, FetchOptions{})
		errCh <- fetchErr
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for server handler")
	}
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestCappedReader(t *testing.T) {
	r := &cappedReader{r: bytes.NewBufferString("hello world"), limit: 5}
	buf := make([]byte, 16)
	if n, err := r.Read(buf); err != nil || n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("first Read = (%d, %q, %v), want (5, %q, nil)", n, string(buf[:n]), err, "hello")
	}
	if _, err := r.Read(buf); !errors.Is(err, ErrProfileTooLarge) || !r.exceeded {
		t.Fatalf("second Read error = %v (exceeded=%v), want ErrProfileTooLarge", err, r.exceeded)
	}
}
