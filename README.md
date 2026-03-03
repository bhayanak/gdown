# gdown — High-Performance Parallel File Downloader

A robust, efficient CLI tool for downloading large files from HTTP(S) endpoints, supporting both single-stream and parallel chunked downloads. Designed for speed, reliability, and usability, with advanced features for power users and automation.

---

## Features

- **Parallel Downloading**: Splits files into chunks and downloads them concurrently for maximum throughput (if the server supports HTTP Range requests).
- **Automatic Fallback**: Gracefully falls back to single-stream mode if the server does not support parallel downloads.
- **Configurable Workers & Buffer Size**: Tune the number of parallel workers and buffer size for optimal performance on any network or hardware.
- **Resilient Retries**: Each chunk is retried independently with exponential backoff and resumes from the last successful byte.
- **Progress Reporting**: Real-time progress, speed, and ETA display for both serial and parallel modes.
- **Proxy Support**: Honors standard HTTP_PROXY, HTTPS_PROXY, and NO_PROXY environment variables.
- **TLS Support**: Works with self-signed certificates (TLS verification disabled by default).
- **Safe File Handling**: Pre-allocates output file, cleans up on failure, and fsyncs on completion.

---

## How to Compile and Run

### 1. Build the Binary

- **macOS / Linux:**
  ```sh
  go build -o gdown main.go
  ```
- **Windows:**
  ```sh
  go build -o gdown.exe main.go
  ```

### 2. Run the Downloader

- **macOS / Linux:**
  ```sh
  ./gdown -uri <URL> -outfile <path> [OPTIONS]
  ```
- **Windows:**
  ```sh
  gdown.exe -uri <URL> -outfile <path> [OPTIONS]
  ```

---

## Usage

```
gdown -uri <URL> -outfile <path> [OPTIONS]
```

### Required Flags

- `-uri`      URL of the file to download
- `-outfile`  Destination path to save the downloaded file

### Options

- `-parallel`         Enable parallel chunked downloading (default: off)
- `-workers <n>`      Number of concurrent chunk workers (default: 16)
- `-retries <n>`      Max retry attempts per chunk (default: 3)
- `-bufsize <MB>`     Per-worker read-buffer size in MB (default: 4)
- `-useProxy`         Use HTTP_PROXY/HTTPS_PROXY/NO_PROXY from environment
- `-version`          Print version and exit

---

## Examples

- **Single-stream download:**
  ```
  gdown -uri https://example.com/file.iso -outfile file.iso
  ```
- **Parallel download (default 16 workers):**
  ```
  gdown -uri https://example.com/bigfile.tar.gz -outfile bigfile.tar.gz -parallel
  ```
- **Aggressive parallel download (64 workers, 8 MB buffers):**
  ```
  gdown -uri https://example.com/dump.img -outfile dump.img -parallel -workers 64 -bufsize 8
  ```
- **Download through a proxy:**
  ```
  HTTPS_PROXY=http://proxy.corp:3128 \
  gdown -uri https://releases.example.com/v2.tar.gz -outfile v2.tar.gz -parallel -useProxy
  ```
- **Parallel download with 5 retries per chunk:**
  ```
  gdown -uri https://flaky-host.com/large.bin -outfile large.bin -parallel -retries 5
  ```

---

## Notes

- Parallel mode pre-allocates the full output file before downloading.
- If the server omits Content-Length or does not support Range requests, `-parallel` automatically falls back to single-stream mode.
- TLS certificate verification is disabled by default to support internal/private endpoints with self-signed certificates.
- Each failed chunk is retried independently; only the missing byte range is re-requested, not the entire chunk.
- Progress and ETA are updated in real time.

---

## Requirements

- Go 1.18 or newer
- Linux, macOS, or Windows

---

## License

MIT License
