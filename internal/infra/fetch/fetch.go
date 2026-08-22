// Package fetch downloads media tracks using parallel ranged requests.
//
// CRITICAL: googlevideo throttles a single sequential GET to ~300 KB/s but
// serves ranged chunks at ~20 MB/s (60x+) — a straight fetch runs slower
// than playback. Chunking is a correctness requirement, not a perf tweak.
package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

type Fetcher struct {
	client     *http.Client
	workers    int
	chunkBytes int64
	maxRetries int
}

func New(workers int, chunkBytes int64) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: workers + 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		workers:    workers,
		chunkBytes: chunkBytes,
		maxRetries: 3,
	}
}

// Fetch downloads t into dest. onProgress may be nil.
func (f *Fetcher) Fetch(ctx context.Context, t video.Track, dest string, onProgress func(done, total int64)) error {
	total := t.SizeBytes
	if total <= 0 {
		total = clenFromURL(t.URL)
	}

	// NOTE: resolve the redirect once and reuse it — re-following a 302
	// per chunk cost ~5x throughput in testing.
	final, probed, err := f.probe(ctx, t.URL)
	if err != nil {
		return err
	}
	if total <= 0 {
		total = probed
	}
	if total <= 0 {
		return fmt.Errorf("cannot determine track size for %s", dest)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(total); err != nil {
		return err
	}

	nChunks := (total + f.chunkBytes - 1) / f.chunkBytes
	var done atomic.Int64
	var next atomic.Int64

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, f.workers)
	var wg sync.WaitGroup
	for w := 0; w < f.workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= nChunks {
					return
				}
				start := i * f.chunkBytes
				end := start + f.chunkBytes - 1
				if end >= total {
					end = total - 1
				}
				n, err := f.chunk(ctx, final, out, start, end)
				if err != nil {
					errs[w] = err
					cancel()
					return
				}
				if onProgress != nil {
					onProgress(done.Add(n), total)
				}
			}
		}(w)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return out.Sync()
}

// chunk fetches one byte range and writes it at its offset, retrying
// transient failures.
func (f *Fetcher) chunk(ctx context.Context, url string, out *os.File, start, end int64) (int64, error) {
	var lastErr error
	for attempt := 0; attempt <= f.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		n, err := f.chunkOnce(ctx, url, out, start, end)
		if err == nil {
			return n, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		lastErr = err
	}
	return 0, fmt.Errorf("range %d-%d after %d retries: %w", start, end, f.maxRetries, lastErr)
}

func (f *Fetcher) chunkOnce(ctx context.Context, url string, out *os.File, start, end int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// CRITICAL: a 200 means the server ignored Range and would stream at
	// the throttled rate — reject it, don't silently accept.
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("expected 206, got %s", resp.Status)
	}

	n, err := io.Copy(io.NewOffsetWriter(out, start), resp.Body)
	if err != nil {
		return n, err
	}
	if want := end - start + 1; n != want {
		return n, fmt.Errorf("short chunk: got %d bytes, want %d", n, want)
	}
	return n, nil
}

// probe follows redirects once and reports the final URL and total size.
func (f *Fetcher) probe(ctx context.Context, raw string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Range", "bytes=0-1")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 8))

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("probe failed: %s", resp.Status)
	}
	final := raw
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return final, totalFromContentRange(resp.Header.Get("Content-Range")), nil
}

// clenFromURL reads googlevideo's clen query parameter, which carries the
// exact track length and saves a round trip (implementation.md §2.4).
func clenFromURL(raw string) int64 {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(u.Query().Get("clen"), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// totalFromContentRange parses the total out of "bytes 0-1/123456".
func totalFromContentRange(v string) int64 {
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == '/' {
			n, err := strconv.ParseInt(v[i+1:], 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}
