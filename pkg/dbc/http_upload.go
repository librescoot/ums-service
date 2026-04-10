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

// dataServerHeaderPrefix is what librescoot-data-server sets in its
// Server: response header — used to distinguish it from a plain
// python3 http.server (which sends BaseHTTP/x.y Python/z.w).
const dataServerHeaderPrefix = "librescoot-data-server/"

// startUploadServer establishes an HTTP PUT endpoint on the DBC for
// fast file transfers. Preference order:
//
//  1. If librescoot-data-server is already running on 8080 (detected
//     via its Server: header), use that directly — no bootstrap needed.
//  2. Otherwise, write /tmp/upload_srv.py over SSH and launch it
//     detached via nohup, matching the installer trampoline pattern.
//  3. If both fail, return error — callers fall through to SCP.
func (i *Interface) startUploadServer(ctx context.Context) error {
	if kind, ok := i.probeUploadServer(ctx); ok {
		i.uploadServerKind = kind
		log.Printf("DBC upload server: using existing %s on %s:%d", kindName(kind), i.ip, uploadServerPort)
		return nil
	}

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

	// Wait up to 10s for the bootstrapped server to come up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if kind, ok := i.probeUploadServer(ctx); ok {
			i.uploadServerKind = kind
			log.Printf("DBC upload server ready on %s:%d (%s)", i.ip, uploadServerPort, kindName(kind))
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("DBC upload server did not become ready within 10s")
}

// probeUploadServer pokes the port with a GET / and classifies the
// response. Returns (kind, true) if something answered, (_, false) if
// nothing is there.
func (i *Interface) probeUploadServer(ctx context.Context) (uploadServerKind, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/", i.ip, uploadServerPort)
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return uploadServerNone, false
	}
	// Accept JSON to nudge data-server into returning its listing
	// instead of the HTML UI (which is content-negotiated).
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return uploadServerNone, false
	}
	defer resp.Body.Close()

	if strings.HasPrefix(resp.Header.Get("Server"), dataServerHeaderPrefix) {
		return uploadServerDataServer, true
	}
	// Any other responder on the port — almost certainly our Python
	// bootstrap. Treat as such; worst case the PUT paths will 404 and
	// we fall through to SCP.
	return uploadServerBootstrapped, true
}

func kindName(k uploadServerKind) string {
	switch k {
	case uploadServerDataServer:
		return "librescoot-data-server"
	case uploadServerBootstrapped:
		return "bootstrapped upload_srv.py"
	default:
		return "none"
	}
}

// stopUploadServer tears down whatever HTTP PUT endpoint we were using.
// For a bootstrapped python server it kills the process and wipes the
// /tmp/upload_srv.* files. For a pre-existing data-server it only runs
// a plain `sync` as a power-cut backstop. For either, the ssh call is
// bounded so Disable() can't hang on an unresponsive DBC.
func (i *Interface) stopUploadServer() {
	if i.uploadServerKind == uploadServerNone {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// `sync` before teardown: data-server's handleWrite already fsyncs
	// file + parent dir, and our bootstrapped python fsyncs the file —
	// but Disable() is about to cut DBC power, so sweep anything that
	// snuck in between the last upload and now.
	var remoteCmd string
	switch i.uploadServerKind {
	case uploadServerDataServer:
		remoteCmd = "sync"
	case uploadServerBootstrapped:
		remoteCmd = "sync; kill $(cat /tmp/upload_srv.pid 2>/dev/null) 2>/dev/null; " +
			"rm -f /tmp/upload_srv.pid /tmp/upload_srv.py /tmp/upload_srv.log"
	}

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", i.ip),
		remoteCmd)
	if err := cmd.Run(); err != nil {
		log.Printf("stopUploadServer: %v (non-fatal)", err)
	}
	i.uploadServerKind = uploadServerNone
}

// ProgressFunc is called with (bytesSent, totalBytes) as an upload advances.
// totalBytes is the full file size; implementations should be cheap as this
// fires after every chunk flush. Declared as a type alias so callers can
// pass a bare `func(int64, int64)` (e.g. from umslog) without conversion.
type ProgressFunc = func(bytesSent, totalBytes int64)

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

// UploadFile streams localPath to the DBC via HTTP PUT against whatever
// upload server startUploadServer settled on. remotePath must start with
// "/". progressCb may be nil.
//
// Much faster than SCP for large files because there's no per-block crypto;
// the installer trampoline uses the same trick for tile uploads.
func (i *Interface) UploadFile(ctx context.Context, localPath, remotePath string, progressCb ProgressFunc) error {
	if !i.enabled {
		return fmt.Errorf("DBC interface not enabled")
	}
	if i.uploadServerKind == uploadServerNone {
		return fmt.Errorf("no DBC upload server available")
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

	// Path semantics differ between the two servers:
	//   - bootstrapped python writes to self.path as an absolute path,
	//     so PUT /data/maps/map.mbtiles → /data/maps/map.mbtiles
	//   - librescoot-data-server joins the request path under -data
	//     (default /data), so PUT /maps/map.mbtiles → /data/maps/map.mbtiles
	// Callers always hand us an absolute filesystem path; translate
	// here rather than burdening each caller with the distinction.
	urlPath := remotePath
	if i.uploadServerKind == uploadServerDataServer {
		urlPath = strings.TrimPrefix(remotePath, "/data")
		if urlPath == "" || urlPath[0] != '/' {
			return fmt.Errorf("data-server mode requires remotePath under /data, got %q", remotePath)
		}
	}
	url := fmt.Sprintf("http://%s:%d%s", i.ip, uploadServerPort, urlPath)

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

// TransferFile sends localPath to remotePath on the DBC. Attempts, in
// order:
//
//  1. HTTP PUT against the detected upload server
//  2. HTTP PUT retry, after re-probing the upload server (covers the
//     data-server systemd restart window and short-lived hiccups)
//  3. SCP fallback
//
// After any failed attempt the (possibly partial) remote file is
// removed via ssh rm -f so the next retry starts clean. progressCb is
// only invoked on the HTTP path. The context bounds the whole
// operation.
func (i *Interface) TransferFile(ctx context.Context, localPath, remotePath string, progressCb ProgressFunc) error {
	// Attempt 1: primary HTTP PUT.
	if err := i.UploadFile(ctx, localPath, remotePath, progressCb); err == nil {
		return nil
	} else {
		log.Printf("HTTP upload of %s failed: %v", localPath, err)
		i.removePartialRemote(remotePath)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Attempt 2: re-probe (the server may have been restarted by
	// systemd or momentarily gone away) and retry HTTP once if
	// something is back on the port.
	if kind, ok := i.probeUploadServer(ctx); ok {
		if kind != i.uploadServerKind {
			log.Printf("DBC upload server changed %s -> %s, retrying", kindName(i.uploadServerKind), kindName(kind))
			i.uploadServerKind = kind
		} else {
			log.Printf("DBC upload server still reachable, retrying once")
		}
		if err := i.UploadFile(ctx, localPath, remotePath, progressCb); err == nil {
			return nil
		} else {
			log.Printf("HTTP upload retry of %s failed: %v", localPath, err)
			i.removePartialRemote(remotePath)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Attempt 3: SCP fallback.
	log.Printf("falling back to SCP for %s", localPath)
	if err := i.CopyFile(ctx, localPath, remotePath); err != nil {
		log.Printf("DBC transfer failed for %s -> %s (all paths exhausted)", localPath, remotePath)
		i.removePartialRemote(remotePath)
		return err
	}
	return nil
}

// removePartialRemote best-effort-deletes a remote file via ssh rm -f.
// Used to wipe partial data between retry attempts so each attempt
// starts from a clean slate. Errors are non-fatal — the file might
// simply not exist, or the DBC might be briefly unreachable, and in
// either case the next step will either succeed over or retry past.
func (i *Interface) removePartialRemote(remotePath string) {
	if remotePath == "" || remotePath == "/" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", i.ip),
		fmt.Sprintf("rm -f %q", remotePath))
	if err := cmd.Run(); err != nil {
		log.Printf("cleanup of partial %s failed (non-fatal): %v", remotePath, err)
	}
}

