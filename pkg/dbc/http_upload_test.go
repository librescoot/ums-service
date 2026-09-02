package dbc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestInterface returns an enabled Interface pointed at 127.0.0.1:port
// with short timeouts so failure paths finish quickly.
func newTestInterface(port int) *Interface {
	return &Interface{
		ip:               "127.0.0.1",
		enabled:          true,
		uploadPort:       port,
		bootstrapTimeout: 2 * time.Second,
		readyTimeout:     2 * time.Second,
	}
}

// serverPort extracts the TCP port an httptest server listens on.
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return port
}

// reservePort finds a free loopback port and releases it again so a
// server can be started on it later in the test.
func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// sshCall records one execCommand invocation.
type sshCall struct {
	name string
	args []string
}

// fakeExec swaps execCommand for the duration of the test. Each
// invocation is recorded and handled by the given function, which
// returns the shell snippet to run in its place.
func fakeExec(t *testing.T, handle func(call int, args []string) string) *[]sshCall {
	t.Helper()
	var mu sync.Mutex
	var calls []sshCall
	prev := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		n := len(calls)
		calls = append(calls, sshCall{name: name, args: args})
		mu.Unlock()
		return exec.CommandContext(ctx, "sh", "-c", handle(n, args))
	}
	t.Cleanup(func() { execCommand = prev })
	return &calls
}

func TestProbeUploadServerClassifiesDataServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "librescoot-data-server/1.2.3")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	i := newTestInterface(serverPort(t, srv))
	kind, ok := i.probeUploadServer(context.Background())
	if !ok || kind != uploadServerDataServer {
		t.Fatalf("got (%v, %v), want (uploadServerDataServer, true)", kind, ok)
	}
}

func TestProbeUploadServerClassifiesOtherResponder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "BaseHTTP/0.6 Python/3.12.0")
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer srv.Close()

	i := newTestInterface(serverPort(t, srv))
	kind, ok := i.probeUploadServer(context.Background())
	if !ok || kind != uploadServerBootstrapped {
		t.Fatalf("got (%v, %v), want (uploadServerBootstrapped, true)", kind, ok)
	}
}

func TestProbeUploadServerNothingListening(t *testing.T) {
	i := newTestInterface(reservePort(t))
	kind, ok := i.probeUploadServer(context.Background())
	if ok || kind != uploadServerNone {
		t.Fatalf("got (%v, %v), want (uploadServerNone, false)", kind, ok)
	}
}

func TestStartUploadServerUsesExistingDataServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "librescoot-data-server/1.2.3")
	}))
	defer srv.Close()

	calls := fakeExec(t, func(int, []string) string { return "exit 1" })
	i := newTestInterface(serverPort(t, srv))
	if err := i.startUploadServer(context.Background()); err != nil {
		t.Fatalf("startUploadServer: %v", err)
	}
	if i.uploadServerKind != uploadServerDataServer {
		t.Fatalf("kind = %v, want uploadServerDataServer", i.uploadServerKind)
	}
	if len(*calls) != 0 {
		t.Fatalf("ssh was invoked %d times, want 0", len(*calls))
	}
}

func TestStartUploadServerBootstrapsOverSSH(t *testing.T) {
	port := reservePort(t)
	scriptPath := filepath.Join(t.TempDir(), "upload_srv.py")

	var srv *http.Server
	var srvMu sync.Mutex
	t.Cleanup(func() {
		srvMu.Lock()
		defer srvMu.Unlock()
		if srv != nil {
			srv.Close()
		}
	})

	// The fake ssh saves stdin where the real one would write the script
	// and then plays the part of the started python by listening on the
	// upload port.
	calls := fakeExec(t, func(call int, args []string) string {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Errorf("listen on reserved port: %v", err)
			return "exit 1"
		}
		srvMu.Lock()
		srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		})}
		srvMu.Unlock()
		go srv.Serve(ln)
		return "cat > " + scriptPath
	})

	i := newTestInterface(port)
	if err := i.startUploadServer(context.Background()); err != nil {
		t.Fatalf("startUploadServer: %v", err)
	}
	if i.uploadServerKind != uploadServerBootstrapped {
		t.Fatalf("kind = %v, want uploadServerBootstrapped", i.uploadServerKind)
	}

	if len(*calls) != 1 {
		t.Fatalf("ssh invoked %d times, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.name != "ssh" {
		t.Fatalf("exec name = %q, want ssh", call.name)
	}
	wantArgs := []string{"-y", "root@127.0.0.1", uploadServerStartCmd}
	if strings.Join(call.args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("ssh args = %q, want %q", call.args, wantArgs)
	}

	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("script was not written over ssh stdin: %v", err)
	}
	for _, want := range []string{
		fmt.Sprintf(`("0.0.0.0", %d)`, port),
		"os.setsid()",
		"/tmp/upload_srv.pid",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(string(script), "PORT") {
		t.Errorf("PORT placeholder was not substituted")
	}
}

func TestUploadServerStartCmdDetaches(t *testing.T) {
	cmd := uploadServerStartCmd
	if !strings.HasPrefix(cmd, "cat > /tmp/upload_srv.py && ") {
		t.Errorf("script write must run in the foreground before the start: %q", cmd)
	}
	start := strings.TrimPrefix(cmd, "cat > /tmp/upload_srv.py && ")
	if !strings.HasPrefix(start, "(") || !strings.HasSuffix(start, " &)") {
		t.Errorf("python must be backgrounded inside a subshell: %q", start)
	}
	for _, want := range []string{"< /dev/null", "> /tmp/upload_srv.log", "2>&1"} {
		if !strings.Contains(start, want) {
			t.Errorf("start command lacks %q: %q", want, start)
		}
	}
}

func TestStartUploadServerKillsHungSSH(t *testing.T) {
	fakeExec(t, func(int, []string) string { return "sleep 30" })

	i := newTestInterface(reservePort(t))
	i.bootstrapTimeout = 300 * time.Millisecond

	begin := time.Now()
	err := i.startUploadServer(context.Background())
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("expected error from hung ssh")
	}
	if !strings.Contains(err.Error(), "failed to start DBC upload server") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hung ssh was not killed at the bootstrap timeout (took %s)", elapsed)
	}
	if i.uploadServerKind != uploadServerNone {
		t.Fatalf("kind = %v, want uploadServerNone", i.uploadServerKind)
	}
}

func TestStartUploadServerReportsNotReadyWithLog(t *testing.T) {
	calls := fakeExec(t, func(call int, args []string) string {
		switch call {
		case 0:
			return "cat > /dev/null"
		default:
			return "echo 'sh: python3: not found'"
		}
	})

	i := newTestInterface(reservePort(t))
	i.readyTimeout = 300 * time.Millisecond

	err := i.startUploadServer(context.Background())
	if err == nil {
		t.Fatal("expected not-ready error")
	}
	for _, want := range []string{"did not become ready", "python3: not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("ssh invoked %d times, want 2 (start + log tail)", len(*calls))
	}
	if got := (*calls)[1].args[2]; !strings.Contains(got, "/tmp/upload_srv.log") {
		t.Errorf("second ssh should read the log, got %q", got)
	}
	if i.uploadServerKind != uploadServerNone {
		t.Fatalf("kind = %v, want uploadServerNone", i.uploadServerKind)
	}
}

func TestStopUploadServerCommands(t *testing.T) {
	cases := []struct {
		kind uploadServerKind
		want string
	}{
		{uploadServerDataServer, "sync"},
		{uploadServerBootstrapped, uploadServerStopCmd},
	}
	for _, tc := range cases {
		calls := fakeExec(t, func(int, []string) string { return "true" })
		i := newTestInterface(reservePort(t))
		i.uploadServerKind = tc.kind
		i.stopUploadServer()
		if len(*calls) != 1 {
			t.Fatalf("%s: ssh invoked %d times, want 1", kindName(tc.kind), len(*calls))
		}
		if got := (*calls)[0].args[2]; got != tc.want {
			t.Errorf("%s: remote cmd = %q, want %q", kindName(tc.kind), got, tc.want)
		}
		if i.uploadServerKind != uploadServerNone {
			t.Errorf("%s: kind not reset", kindName(tc.kind))
		}
	}

	calls := fakeExec(t, func(int, []string) string { return "true" })
	i := newTestInterface(reservePort(t))
	i.stopUploadServer()
	if len(*calls) != 0 {
		t.Errorf("no server: ssh invoked %d times, want 0", len(*calls))
	}
}

// putRecorder is an upload server stand-in that records PUT paths.
type putRecorder struct {
	mu    sync.Mutex
	paths []string
	body  []byte
}

func (p *putRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || int64(len(body)) != r.ContentLength {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.body = body
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadFileDataServerPathGuard(t *testing.T) {
	rec := &putRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	i := newTestInterface(serverPort(t, srv))
	i.uploadServerKind = uploadServerDataServer
	local := writeTempFile(t, "hello")

	for _, bad := range []string{"/tmp/x", "/data", "/datax/y", "/etc/passwd"} {
		err := i.UploadFile(context.Background(), local, bad, nil)
		if err == nil {
			t.Errorf("%s: expected rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "under /data") {
			t.Errorf("%s: unexpected error: %v", bad, err)
		}
	}
	if len(rec.paths) != 0 {
		t.Fatalf("rejected paths reached the server: %v", rec.paths)
	}

	if err := i.UploadFile(context.Background(), local, "/data/x", nil); err != nil {
		t.Fatalf("UploadFile /data/x: %v", err)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/x" {
		t.Fatalf("PUT paths = %v, want [/x]", rec.paths)
	}
	if string(rec.body) != "hello" {
		t.Fatalf("body = %q, want hello", rec.body)
	}
}

func TestUploadFileBootstrappedUsesAbsolutePath(t *testing.T) {
	rec := &putRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	i := newTestInterface(serverPort(t, srv))
	i.uploadServerKind = uploadServerBootstrapped
	local := writeTempFile(t, "hello")

	var sent, total int64
	progress := func(s, tot int64) { sent, total = s, tot }
	if err := i.UploadFile(context.Background(), local, "/data/maps/x", progress); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "/data/maps/x" {
		t.Fatalf("PUT paths = %v, want [/data/maps/x]", rec.paths)
	}
	if sent != 5 || total != 5 {
		t.Fatalf("progress = (%d, %d), want (5, 5)", sent, total)
	}
	if err := i.UploadFile(context.Background(), local, "/tmp/x", nil); err != nil {
		t.Fatalf("bootstrapped server must accept paths outside /data: %v", err)
	}
}

func TestUploadFilePreconditions(t *testing.T) {
	local := writeTempFile(t, "hello")

	i := newTestInterface(reservePort(t))
	if err := i.UploadFile(context.Background(), local, "/data/x", nil); err == nil ||
		!strings.Contains(err.Error(), "no DBC upload server") {
		t.Errorf("kind none: got %v", err)
	}

	i.uploadServerKind = uploadServerBootstrapped
	if err := i.UploadFile(context.Background(), local, "relative/x", nil); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path: got %v", err)
	}

	i.enabled = false
	if err := i.UploadFile(context.Background(), local, "/data/x", nil); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Errorf("disabled: got %v", err)
	}
}
