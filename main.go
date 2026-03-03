package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultWorkers   = 16
	defaultBufSizeMB = 4               // MB
	defaultRetries   = 3
	minChunkSize     = 1 * 1024 * 1024 // 1 MB — don't create workers for smaller slices
	progressInterval = 500 * time.Millisecond
)

var version = "1.0"

// ── CLI ──────────────────────────────────────────────────────────────────────

func main() {
	var (
		uri     string
		outfile string
		parallel bool
		useProxy bool
		workers  int
		retries  int
		bufMB    int
		showVer  bool
	)

	flag.StringVar(&uri, "uri", "", "URL of the file to download (required)")
	flag.StringVar(&outfile, "outfile", "", "Destination file path for the download (required)")
	flag.BoolVar(&parallel, "parallel", false, "Enable parallel chunked downloading (server must support Range requests)")
	flag.BoolVar(&useProxy, "useProxy", false, "Honour HTTP_PROXY / HTTPS_PROXY / NO_PROXY environment variables")
	flag.IntVar(&workers, "workers", defaultWorkers, "Number of concurrent download workers (used with -parallel)")
	flag.IntVar(&retries, "retries", defaultRetries, "Retry attempts per chunk on transient failure (used with -parallel)")
	flag.IntVar(&bufMB, "bufsize", defaultBufSizeMB, "Read-buffer size in MB per worker")
	flag.BoolVar(&showVer, "version", false, "Print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `
gdown — High-performance parallel file downloader

VERSION:
  %s

USAGE:
  gdown -uri <URL> -outfile <path> [OPTIONS]

REQUIRED FLAGS:
  -uri      <URL>   URL of the file to download
  -outfile  <path>  Destination path to save the downloaded file

OPTIONS:
  -parallel         Enable parallel chunked downloading.
                    Splits the file into equal ranges and fetches them
                    concurrently. The server must support HTTP Range requests.
                    Falls back to serial automatically if not supported.

  -workers  <n>     Number of concurrent chunk workers (default: %d).
                    Effective only with -parallel.

  -retries  <n>     Max retry attempts per chunk on transient error (default: %d).
                    Each retry resumes from the byte where the previous
                    attempt failed, with exponential back-off.

  -bufsize  <MB>    Per-worker read-buffer size in MB (default: %d MB).
                    Larger values reduce syscall overhead on fast links.

  -useProxy         Read proxy settings from the environment:
                      HTTP_PROXY, HTTPS_PROXY, NO_PROXY

  -version          Print version and exit.

EXAMPLES:
  # Single-stream download (simplest):
  gdown -uri https://example.com/file.iso -outfile file.iso

  # Parallel download with default 16 workers:
  gdown -uri https://example.com/bigfile.tar.gz -outfile bigfile.tar.gz -parallel

  # Aggressive parallel download — 64 workers, 8 MB buffers:
  gdown -uri https://example.com/dump.img -outfile dump.img -parallel -workers 64 -bufsize 8

  # Download through a corporate proxy:
  HTTPS_PROXY=http://proxy.corp:3128 \
  gdown -uri https://releases.example.com/v2.tar.gz -outfile v2.tar.gz -parallel -useProxy

  # Parallel download with 5 retries per chunk:
  gdown -uri https://flaky-host.com/large.bin -outfile large.bin -parallel -retries 5

NOTES:
  • Parallel mode pre-allocates the full output file before downloading,
    so peak disk usage equals the target file size from the start.
  • If the server omits Content-Length or does not support Range requests,
    -parallel automatically falls back to single-stream mode.
  • TLS certificate verification is disabled to support internal/private
    endpoints with self-signed certificates.
  • Each failed chunk is retried independently — only the missing byte
    range is re-requested, not the entire chunk.

`, version, defaultWorkers, defaultRetries, defaultBufSizeMB)
	}

	flag.Parse()

	if showVer {
		fmt.Printf("gdown version %s\n", version)
		return
	}

	// Validate required flags and give actionable feedback.
	var missing []string
	if uri == "" {
		missing = append(missing, "-uri")
	}
	if outfile == "" {
		missing = append(missing, "-outfile")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "Error: required flag(s) not provided: %s\n\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(1)
	}

	if workers < 1 {
		fmt.Fprintln(os.Stderr, "Error: -workers must be >= 1")
		os.Exit(1)
	}
	if retries < 0 {
		fmt.Fprintln(os.Stderr, "Error: -retries must be >= 0")
		os.Exit(1)
	}

	bufBytes := int64(bufMB) * 1024 * 1024

	// ── HTTP transport ──────────────────────────────────────────────────────
	// • Explicit dialer with TCP keep-alive keeps long-lived connections stable.
	// • DisableCompression avoids needless CPU decompression on binary data.
	// • ForceAttemptHTTP2 enables HTTP/2 multiplexing when the server supports it.
	// • MaxIdleConnsPerHost matches worker count so every goroutine can reuse
	//   an idle connection on retry without opening a new socket.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxConnsPerHost:       workers + 4,
		MaxIdleConns:          workers,
		MaxIdleConnsPerHost:   workers,
		ReadBufferSize:        int(bufBytes),
		WriteBufferSize:       2 * 1024 * 1024,
		DisableCompression:    true, // avoid decompression CPU overhead on binary files
		Proxy:                 nil,
	}
	if useProxy {
		transport.Proxy = http.ProxyFromEnvironment
	}

	http.DefaultClient = &http.Client{
		Timeout:   0,
		Transport: transport,
	}

	cfg := config{
		uri:      uri,
		outfile:  outfile,
		workers:  workers,
		retries:  retries,
		bufBytes: bufBytes,
	}

	var err error
	if parallel {
		err = parallelDownload(cfg)
	} else {
		err = serialDownload(cfg)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
}

// ── Config ────────────────────────────────────────────────────────────────────

type config struct {
	uri      string
	outfile  string
	workers  int
	retries  int
	bufBytes int64
}

// ── Serial download ───────────────────────────────────────────────────────────

func serialDownload(cfg config) error {
	resp, err := http.Get(cfg.uri)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}

	var totalSize int64
	if s := resp.Header.Get("Content-Length"); s != "" {
		totalSize, _ = strconv.ParseInt(s, 10, 64)
	}

	file, err := os.Create(cfg.outfile)
	if err != nil {
		return fmt.Errorf("cannot create output file: %w", err)
	}
	defer file.Close()

	var downloaded int64
	startTime := time.Now()
	buf := make([]byte, cfg.bufBytes)

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := file.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write error: %w", werr)
			}
			downloaded += int64(n)
			printProgress(downloaded, totalSize, startTime)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read error: %w", err)
		}
	}

	// Flush OS buffers to disk before reporting success.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("file sync failed: %w", err)
	}
	fmt.Printf("\nDownload complete: %s\n", cfg.outfile)
	return nil
}

// ── Parallel download ─────────────────────────────────────────────────────────

func parallelDownload(cfg config) error {
	// HEAD to learn file size and whether the server accepts Range requests.
	resp, err := http.Head(cfg.uri)
	if err != nil {
		return fmt.Errorf("HEAD request failed: %w", err)
	}
	resp.Body.Close()

	size, parseErr := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if parseErr != nil || size <= 0 {
		fmt.Fprintln(os.Stderr, "Warning: Content-Length unavailable — falling back to serial download.")
		return serialDownload(cfg)
	}

	if !strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
		fmt.Fprintln(os.Stderr, "Warning: server does not advertise Range support — falling back to serial download.")
		return serialDownload(cfg)
	}

	// Clamp worker count so each chunk is at least minChunkSize bytes.
	workers := cfg.workers
	if chunkFloor := int(size / minChunkSize); chunkFloor < workers {
		workers = chunkFloor
		if workers < 1 {
			workers = 1
		}
	}

	// Pre-allocate the output file so parallel WriteAt calls never race on
	// file growth and the OS can lay out the data contiguously.
	file, err := os.Create(cfg.outfile)
	if err != nil {
		return fmt.Errorf("cannot create output file: %w", err)
	}
	defer file.Close()

	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("file pre-allocation failed: %w", err)
	}

	// Context lets a single failing worker cancel all the others immediately.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var downloaded int64
	startTime := time.Now()
	chunkSize := size / int64(workers)

	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == workers-1 {
			end = size - 1 // last worker takes any remainder
		}
		go func(start, end int64) {
			defer wg.Done()
			if err := downloadChunkWithRetry(ctx, cfg, file, start, end, &downloaded); err != nil {
				cancel() // stop all other workers
				errCh <- err
			}
		}(start, end)
	}

	// Progress ticker — fires every 500 ms, stops cleanly via done channel.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				printProgressParallel(atomic.LoadInt64(&downloaded), size, startTime)
			}
		}
	}()

	wg.Wait()
	close(done)
	close(errCh)

	// Collect and combine any worker errors.
	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		_ = os.Remove(cfg.outfile) // remove incomplete file
		return fmt.Errorf("parallel download failed: %w", errors.Join(errs...))
	}

	// Print a final 100 % progress line.
	printProgressParallel(atomic.LoadInt64(&downloaded), size, startTime)

	if err := file.Sync(); err != nil {
		return fmt.Errorf("file sync failed: %w", err)
	}
	fmt.Printf("\nDownload complete: %s\n", cfg.outfile)
	return nil
}

// ── Chunk helpers ─────────────────────────────────────────────────────────────

// downloadChunkWithRetry downloads the byte range [start, end] with
// exponential back-off retries. On a partial failure it advances the
// start offset so only the missing tail is re-requested.
func downloadChunkWithRetry(
	ctx context.Context,
	cfg config,
	file *os.File,
	start, end int64,
	downloaded *int64,
) error {
	offset := start // tracks how far we have successfully written
	for attempt := 0; ; attempt++ {
		// Bail out early if another worker already cancelled the context.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		written, err := downloadChunk(ctx, cfg, file, offset, end, downloaded)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if attempt >= cfg.retries {
			return fmt.Errorf("chunk bytes=%d-%d failed after %d attempt(s): %w", start, end, attempt+1, err)
		}

		// Advance past bytes already written so the retry only fetches the
		// remainder — avoids re-downloading and double-counting progress.
		offset += written

		backoff := time.Duration(math.Pow(2, float64(attempt+1))) * 200 * time.Millisecond
		fmt.Fprintf(os.Stderr,
			"\nChunk bytes=%d-%d attempt %d/%d failed: %v — retrying in %s\n",
			start, end, attempt+1, cfg.retries, err, backoff.Round(time.Millisecond),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// downloadChunk fetches [start, end] bytes from the server and writes them
// to file at the correct offset. Returns bytes written and any error.
func downloadChunk(
	ctx context.Context,
	cfg config,
	file *os.File,
	start, end int64,
	downloaded *int64,
) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.uri, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("unexpected HTTP status %d for Range request", resp.StatusCode)
	}

	buf := make([]byte, cfg.bufBytes)
	offset := start
	var written int64

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := file.WriteAt(buf[:n], offset); werr != nil {
				return written, fmt.Errorf("write error at offset %d: %w", offset, werr)
			}
			offset += int64(n)
			written += int64(n)
			atomic.AddInt64(downloaded, int64(n))
		}
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}

// ── Progress display ──────────────────────────────────────────────────────────

func printProgress(downloaded, total int64, start time.Time) {
	elapsed := time.Since(start).Seconds()
	if elapsed < 0.01 {
		return
	}
	speed := float64(downloaded) / 1024.0 / 1024.0 / elapsed
	if total > 0 {
		pct := float64(downloaded) / float64(total) * 100
		fmt.Printf("\rDownloaded: %s / %s (%.1f%%) | Speed: %.2f MB/s",
			formatBytes(downloaded), formatBytes(total), pct, speed)
	} else {
		fmt.Printf("\rDownloaded: %s | Speed: %.2f MB/s", formatBytes(downloaded), speed)
	}
}

func printProgressParallel(downloaded, total int64, start time.Time) {
	elapsed := time.Since(start).Seconds()
	if elapsed < 0.1 {
		return
	}
	speed := float64(downloaded) / 1024.0 / 1024.0 / elapsed
	pct := float64(downloaded) / float64(total) * 100

	var eta string
	if speed > 0 {
		remaining := float64(total-downloaded) / 1024.0 / 1024.0 // MB
		etaDur := time.Duration(remaining/speed*1000) * time.Millisecond
		eta = formatDuration(etaDur)
	} else {
		eta = "—"
	}

	// Trailing spaces overwrite any leftover characters from a longer previous line.
	fmt.Printf("\rDownloaded: %s / %s (%.1f%%) | Speed: %.2f MB/s | ETA: %s     ",
		formatBytes(downloaded), formatBytes(total), pct, speed, eta)
}

// ── Formatting helpers ────────────────────────────────────────────────────────

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}