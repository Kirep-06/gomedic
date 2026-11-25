package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPprofPrefix       = "/debug/pprof"
	defaultRequestTimeout    = 30 * time.Second
	defaultCPUProfileSeconds = 30
	maxProfileBytes          = 128 << 20 // 128 MiB safety cap
	maxErrorBodyBytes        = 512
	maxErrorDrainBytes       = 4 << 10
)

// ErrProfileUnsupported indicates that the target does not expose the requested pprof endpoint (HTTP 404).
var ErrProfileUnsupported = errors.New("profile endpoint not supported")

// ErrProfileTooLarge indicates that a pprof response exceeded the safety size cap.
var ErrProfileTooLarge = errors.New("profile response exceeds maximum allowed size")

// HTTPStatusError represents a non-200 HTTP response from a pprof endpoint.
type HTTPStatusError struct {
	URL        string
	StatusCode int
	Body       string
}

// Error implements the error interface.
func (e *HTTPStatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("pprof endpoint %s returned HTTP %d: %s", e.URL, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("pprof endpoint %s returned HTTP %d", e.URL, e.StatusCode)
}

// Unwrap allows HTTP 404 responses to match ErrProfileUnsupported via errors.Is.
func (e *HTTPStatusError) Unwrap() error {
	if e.StatusCode == http.StatusNotFound {
		return ErrProfileUnsupported
	}
	return nil
}

// FetchOptions configures optional query parameters for a single pprof request.
type FetchOptions struct {
	// Seconds sets the sampling duration (?seconds=N) for CPU profiles.
	Seconds int
	// GC requests a runtime GC before capturing a heap profile (?gc=1).
	GC bool
}

// HTTPClient fetches pprof profiles from a live Go net/http/pprof server.
type HTTPClient struct {
	baseURL *url.URL
	client  *http.Client
}

// NewHTTPClient creates an HTTPClient for the given target URL.
// The URL must include an http or https scheme (e.g. "http://127.0.0.1:6060" or
// "http://127.0.0.1:6060/debug/pprof"). If httpClient is nil, a default client is used.
func NewHTTPClient(rawURL string, httpClient *http.Client) (*HTTPClient, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, errors.New("target URL is empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse target URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q (expected http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("target URL %q is missing host", rawURL)
	}

	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	cleanPath := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(cleanPath, defaultPprofPrefix) {
		cleanPath += defaultPprofPrefix
	}
	u.Path = cleanPath

	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return &HTTPClient{
		baseURL: u,
		client:  httpClient,
	}, nil
}

func (c *HTTPClient) buildEndpointURL(kind ProfileKind, opts FetchOptions) (*url.URL, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("unsupported profile kind %q", kind)
	}
	if opts.Seconds < 0 {
		return nil, fmt.Errorf("invalid sampling seconds %d", opts.Seconds)
	}

	u := *c.baseURL
	u.Path = c.baseURL.Path + "/" + kind.Endpoint()
	u.RawPath = ""

	q := url.Values{}
	if kind == ProfileCPU && opts.Seconds > 0 {
		q.Set("seconds", strconv.Itoa(opts.Seconds))
	}
	if kind == ProfileHeap && opts.GC {
		q.Set("gc", "1")
	}
	u.RawQuery = q.Encode()

	return &u, nil
}

// EndpointURL builds the full request URL for a given profile kind and options.
func (c *HTTPClient) EndpointURL(kind ProfileKind, opts FetchOptions) (string, error) {
	u, err := c.buildEndpointURL(kind, opts)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Fetch retrieves and parses a single pprof profile from the target server.
func (c *HTTPClient) Fetch(ctx context.Context, kind ProfileKind, opts FetchOptions) (*Snapshot, error) {
	u, err := c.buildEndpointURL(kind, opts)
	if err != nil {
		return nil, err
	}
	endpoint := u.String()
	redacted := u.Redacted()

	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := defaultRequestTimeout
		if kind == ProfileCPU {
			sampleSeconds := opts.Seconds
			if sampleSeconds == 0 {
				sampleSeconds = defaultCPUProfileSeconds
			}
			timeout += time.Duration(sampleSeconds) * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build pprof request for %s: %w", kind, err)
	}
	req.Header.Set("User-Agent", "gomedic")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s profile from %s: %w", kind, redacted, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorDrainBytes))
		return nil, &HTTPStatusError{
			URL:        redacted,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(snippet)),
		}
	}

	capped := &cappedReader{r: resp.Body, limit: maxProfileBytes}
	p, err := Parse(capped)
	if err != nil {
		if capped.exceeded {
			return nil, fmt.Errorf("decode %s profile from %s: %w", kind, redacted, ErrProfileTooLarge)
		}
		return nil, fmt.Errorf("decode %s profile from %s: %w", kind, redacted, err)
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

// cappedReader stops reading once more than limit bytes have been observed,
// returning ErrProfileTooLarge instead of silently truncating the stream.
type cappedReader struct {
	r        io.Reader
	limit    int64
	read     int64
	exceeded bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.exceeded {
		return 0, ErrProfileTooLarge
	}

	remaining := c.limit - c.read
	if remaining == 0 {
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			c.exceeded = true
			return 0, ErrProfileTooLarge
		}
		return 0, err
	}

	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}
