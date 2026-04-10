package dbc

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// uploadServerPort is where the Python PUT server listens on the DBC.
const uploadServerPort = 8080

// uploadServerScript is a minimal http.server-based PUT endpoint that writes
// each incoming request body straight to the URL path on disk. Matches the
// installer trampoline (installer/assets/trampoline.sh.template:645-673) so
// behavior is identical for large-file uploads.
const uploadServerScript = `import http.server
import os
import sys

class H(http.server.BaseHTTPRequestHandler):
    def do_PUT(self):
        p = self.path
        try:
            d = os.path.dirname(p)
            if d:
                os.makedirs(d, exist_ok=True)
            length = int(self.headers["Content-Length"])
            with open(p, "wb") as f:
                remaining = length
                while remaining > 0:
                    chunk = self.rfile.read(min(65536, remaining))
                    if not chunk:
                        break
                    f.write(chunk)
                    remaining -= len(chunk)
                # Refuse to report success on a short read — the client's
                # Content-Length promised more than the socket actually
                # delivered, so the file is incomplete.
                if remaining > 0:
                    raise IOError("short read: %d bytes missing" % remaining)
                # Force the data all the way to stable storage before the
                # MDB power-cycles the DBC at the end of the UMS processing
                # cycle. Without this the last tens of MB sit in the page
                # cache and get lost on dashboard:off.
                f.flush()
                os.fsync(f.fileno())
            self.send_response(200)
            self.end_headers()
        except Exception as e:
            self.send_response(500)
            self.end_headers()
            try:
                self.wfile.write(str(e).encode())
            except Exception:
                pass

    def log_message(self, *a):
        pass

with open("/tmp/upload_srv.pid", "w") as pf:
    pf.write(str(os.getpid()))

http.server.HTTPServer(("0.0.0.0", PORT), H).serve_forever()
`

// startUploadServer writes the Python PUT server to /tmp/upload_srv.py on the
// DBC and launches it detached via nohup. Returns once the server is ready to
// accept connections (or the timeout elapses).
func (i *Interface) startUploadServer(ctx context.Context) error {
	script := strings.Replace(uploadServerScript, "PORT", fmt.Sprintf("%d", uploadServerPort), 1)

	// Write the script (cat runs foreground so it actually reads stdin),
	// THEN background the python server. The `&` must only apply to nohup —
	// if it covers the `cat &&` chain, the shell backgrounds the whole thing,
	// closes the ssh session's stdin immediately, and cat writes an empty file.
	// `< /dev/null` on python prevents it from holding the ssh channel open.
	remoteCmd := "cat > /tmp/upload_srv.py; " +
		"nohup python3 /tmp/upload_srv.py > /tmp/upload_srv.log 2>&1 < /dev/null &"

	sshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(sshCtx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", i.ip),
		remoteCmd)
	cmd.Stdin = strings.NewReader(script)

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to start DBC upload server: %w (output: %s)", err, string(out))
	}

	// Wait up to 10s for the port to accept a connection.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i.uploadServerReady() {
			log.Printf("DBC upload server ready on %s:%d", i.ip, uploadServerPort)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("DBC upload server did not become ready within 10s")
}

// uploadServerReady pokes the PUT endpoint to see if the server is up.
// A HEAD-like request will 501 but that's fine — we just want the TCP accept.
func (i *Interface) uploadServerReady() bool {
	url := fmt.Sprintf("http://%s:%d/", i.ip, uploadServerPort)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// stopUploadServer kills the Python server on the DBC and removes its pidfile.
// Safe to call even if the server was never started. Bounded by a short
// deadline so Disable() can't hang on an unresponsive DBC.
func (i *Interface) stopUploadServer() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// `sync` before the kill is a belt-and-braces backstop in case any
	// recent upload hasn't been fsync'd yet — Disable() is about to cut
	// DBC power, and any dirty pages on the eMMC would be lost.
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", i.ip),
		"sync; kill $(cat /tmp/upload_srv.pid 2>/dev/null) 2>/dev/null; rm -f /tmp/upload_srv.pid /tmp/upload_srv.py /tmp/upload_srv.log")
	if err := cmd.Run(); err != nil {
		log.Printf("stopUploadServer: %v (non-fatal)", err)
	}
}

// ProgressFunc is called with (bytesSent, totalBytes) as an upload advances.
// totalBytes is the full file size; implementations should be cheap as this
// fires after every chunk flush.
type ProgressFunc func(bytesSent, totalBytes int64)

// progressReader wraps an io.Reader and fires progressFn after each read.
type progressReader struct {
	r        io.Reader
	total    int64
	sent     int64
	progress ProgressFunc
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.sent += int64(n)
		if pr.progress != nil {
			pr.progress(pr.sent, pr.total)
		}
	}
	return n, err
}

// UploadFile streams localPath to the DBC via HTTP PUT against the upload
// server bootstrapped by startUploadServer. remotePath must start with "/".
// progressCb may be nil.
//
// Much faster than SCP for large files because there's no per-block crypto;
// the installer trampoline uses the same trick for tile uploads.
func (i *Interface) UploadFile(ctx context.Context, localPath, remotePath string, progressCb ProgressFunc) error {
	if !i.enabled {
		return fmt.Errorf("DBC interface not enabled")
	}
	if !strings.HasPrefix(remotePath, "/") {
		return fmt.Errorf("remotePath must be absolute, got %q", remotePath)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", localPath, err)
	}
	size := st.Size()

	url := fmt.Sprintf("http://%s:%d%s", i.ip, uploadServerPort, remotePath)

	body := &progressReader{r: f, total: size, progress: progressCb}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{
		// No client-side timeout — callers wrap in ctx with an appropriate
		// per-operation budget (see future error-handling hardening work).
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("PUT %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	elapsed := time.Since(start)
	mbps := float64(size) / elapsed.Seconds() / (1024 * 1024)
	log.Printf("Uploaded %s → DBC:%s (%d bytes in %s, %.1f MB/s)",
		localPath, remotePath, size, elapsed.Truncate(time.Millisecond), mbps)
	return nil
}

// TransferFile sends localPath to remotePath on the DBC. Tries the fast
// HTTP-PUT path first and falls back to SCP on failure. progressCb may be
// nil and is only invoked on the HTTP path. The context bounds the whole
// operation — both the HTTP attempt and the SCP fallback. On failure, the
// remote path is logged so a caller can chase down any partial file left
// on the DBC.
func (i *Interface) TransferFile(ctx context.Context, localPath, remotePath string, progressCb ProgressFunc) error {
	if err := i.UploadFile(ctx, localPath, remotePath, progressCb); err == nil {
		return nil
	} else {
		log.Printf("HTTP upload of %s failed, falling back to SCP: %v", localPath, err)
	}
	if err := i.CopyFile(ctx, localPath, remotePath); err != nil {
		log.Printf("DBC transfer failed for %s -> %s (partial file may remain on DBC)", localPath, remotePath)
		return err
	}
	return nil
}

