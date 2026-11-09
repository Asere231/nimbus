package funcs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A zip entry is a NAME, not a path that can be trusted. "../../etc/passwd"
// is a legal entry, and extracting it with filepath.Join leaves the
// directory entirely -- the same trap as an S3 key, refused the same way.
// Every name here must be refused on EVERY platform. The old check deferred
// to filepath, and the two hosts disagreed: "/absolute.py" is not absolute on
// Windows (no volume name), so CI accepted an entry this test says to refuse,
// while "C:/x" is absolute on Windows and an ordinary name on Unix. A package
// that deploys on one machine and is refused on another is the bug; which way
// round matters less.
func TestUnpackRefusesAnEntryThatEscapesTheDirectory(t *testing.T) {
	for _, name := range []string{
		"../escape.py", "../../etc/passwd", "/absolute.py",
		"nested/../../escape.py",
		`..\..\escape.py`, // backslashes: separators on Windows only
		"C:/absolute.py",  // a drive letter, absolute on Windows only
	} {
		archive := zipWith(t, map[string]string{name: "x = 1\n"})
		dir := filepath.Join(t.TempDir(), "code")
		if err := Unpack(archive, dir); err == nil {
			t.Errorf("%q was accepted", name)
		} else if !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
			// Quoted, because %q escapes a backslash and the raw name would
			// not be a substring of its own error message.
			t.Errorf("the error must name the entry it refused: %v", err)
		}
	}
}

// The drive-letter rule must not swallow a colon used as an ordinary
// character, which is legal in a zip name and in an S3 key.
func TestUnpackAcceptsAColonThatIsNotADriveLetter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "code")
	if err := Unpack(zipWith(t, map[string]string{"a:b.py": "x = 1\n"}), dir); err != nil {
		t.Fatalf("a legal name with a colon was refused: %v", err)
	}
}

func TestUnpackWritesTheCodeAndKeepsTheExecutableBit(t *testing.T) {
	archive := zipExec(t, "bootstrap", "#!/bin/sh\necho hi\n")
	dir := filepath.Join(t.TempDir(), "code")
	if err := Unpack(archive, dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no executable bit: a file mode there drives only the
	// read-only flag, so this comes out as -rw-rw-rw- however it was
	// written. Nothing is lost -- the bit matters inside the Linux
	// container that runs the handler, and the host it was unpacked on
	// cannot express it. Skipped rather than weakened, so the assertion
	// stays exact where it can be made.
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes cannot carry an executable bit")
	}
	// A custom runtime's bootstrap has to stay runnable, or the container
	// fails with "exec format error" and names nothing useful.
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bootstrap came out as %v, not executable", info.Mode().Perm())
	}
}

// An update must not leave a file the new package dropped.
func TestUnpackEmptiesTheDirectoryFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "code")
	if err := Unpack(zipWith(t, map[string]string{"old.py": "x\n"}), dir); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(zipWith(t, map[string]string{"new.py": "y\n"}), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.py")); !os.IsNotExist(err) {
		t.Error("a file the new package does not contain survived the update")
	}
}

// What the engine asks Docker for: the code at /var/task, the handler as the
// command, the environment, and a published port for the emulator.
func TestStartMountsTheCodeAndPassesTheHandler(t *testing.T) {
	f := docker.NewFake()
	f.Images["public.ecr.aws/lambda/python:3.12"] = true
	e := New(Config{NamePrefix: "nimbus-lambda", PortBase: 9400,
		StartTimeout: 50 * time.Millisecond, Dir: t.TempDir()}, f)

	fn := &Function{
		ResourceID: "arn:aws:lambda:us-east-1:000000000000:function:writer",
		Name:       "writer",
		Image:      "public.ecr.aws/lambda/python:3.12",
		Command:    []string{"app.handler"},
		Dir:        "/host/code",
		Env:        map[string]string{"TABLE": "items"},
	}
	// The readiness probe talks to a real port, so this times out by
	// design; what is under test is the container it asked for.
	_ = e.Start(context.Background(), fn)

	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, "/host/code:/var/task") {
		t.Errorf("the code was not mounted at /var/task:\n%s", calls)
	}
	if !strings.Contains(calls, "args=[app.handler]") {
		t.Errorf("the handler was not passed as the command:\n%s", calls)
	}
	containers := f.Containers()
	if len(containers) != 1 {
		t.Fatalf("containers = %v", containers)
	}
	if got := containers[0].Labels[docker.LabelResource]; got != fn.ResourceID {
		t.Errorf("the container is not labelled with its resource: %v", containers[0].Labels)
	}
	if fn.Env["TABLE"] != "items" {
		t.Errorf("env = %v", fn.Env)
	}
}

// A function whose container never answers must FAIL with the reason, not
// hang: Terraform waiting forever is the worse failure.
func TestStartGivesUpWithAReasonThatNamesTheContainer(t *testing.T) {
	f := docker.NewFake()
	f.Images["img"] = true
	e := New(Config{PortBase: 9500, StartTimeout: 50 * time.Millisecond, Dir: t.TempDir()}, f)
	fn := &Function{ResourceID: "arn:fn", Name: "slow", Image: "img", Dir: t.TempDir()}
	err := e.Start(context.Background(), fn)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), fn.Container) {
		t.Errorf("the error must name the container to look at: %v", err)
	}
	if e.Get(fn.ResourceID) != nil {
		t.Error("a function that never started must not be registered as running")
	}
}

// The readiness probe must not INVOKE anything.
//
// It used to, and that ran the handler once at deploy time: a function with
// side effects performed them before anybody asked. Measured against RIE, a
// GET to a path it does not serve answers 404 the moment the port is open
// and does not even import the handler's module.
func TestTheReadinessProbeDoesNotInvokeTheHandler(t *testing.T) {
	invocations := 0
	var probed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/invocations") {
			invocations++
		} else {
			probed = r.Method + " " + r.URL.Path
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	f := docker.NewFake()
	f.Images["img"] = true
	e := New(Config{PortBase: portOf(t, srv.URL), StartTimeout: 2 * time.Second,
		Dir: t.TempDir()}, f)
	fn := &Function{ResourceID: "arn:fn", Name: "writer", Image: "img", Dir: t.TempDir()}
	// PortBase is the server's own port, which FreePort finds taken and
	// skips -- so point the probe at it directly instead.
	fn.Port = portOf(t, srv.URL)
	if err := e.waitReady(context.Background(), fn); err != nil {
		t.Fatalf("a listening server must read as ready: %v", err)
	}
	if invocations != 0 {
		t.Errorf("the probe invoked the handler %d times", invocations)
	}
	if probed == "" || !strings.HasPrefix(probed, "GET ") {
		t.Errorf("the probe was %q, want a GET", probed)
	}
}

// Invoke's contract: the payload goes through untouched, the response body
// comes back untouched, and whether the handler THREW is a header rather
// than part of the body.
func TestInvokePassesThePayloadThroughAndReadsTheErrorHeader(t *testing.T) {
	var got []byte
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = readAll(r)
		path = r.URL.Path
		w.Header().Set("X-Amz-Function-Error", "Unhandled")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"errorMessage":"boom","errorType":"ValueError"}`))
	}))
	defer srv.Close()

	f := docker.NewFake()
	f.Images["img"] = true
	e := New(Config{PortBase: 9600, Dir: t.TempDir()}, f)
	fn := &Function{ResourceID: "arn:fn", Name: "writer", Image: "img",
		Container: "nimbus-lambda-writer", Port: portOf(t, srv.URL)}
	// Logs come from the container, which is the Fake here.
	if _, err := f.Run(context.Background(), docker.RunOptions{Name: fn.Container, Image: "img"}); err != nil {
		t.Fatal(err)
	}
	f.LogOutput[fn.Container] = []byte("hello from the handler\n")

	result, err := e.Invoke(context.Background(), fn, []byte(`{"id":"1"}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The literal "function" in the middle is RIE's, not a placeholder for
	// the function's name.
	if path != "/2015-03-31/functions/function/invocations" {
		t.Errorf("invoked %q", path)
	}
	if string(got) != `{"id":"1"}` {
		t.Errorf("the payload arrived as %q", got)
	}
	if !strings.Contains(string(result.Payload), "boom") {
		t.Errorf("payload = %s", result.Payload)
	}
	if result.FunctionError != "Unhandled" {
		t.Errorf("FunctionError = %q; a raised exception is a header, not a payload shape",
			result.FunctionError)
	}
	if !strings.Contains(result.Logs, "hello from the handler") {
		t.Errorf("logs = %q", result.Logs)
	}
}

// An empty payload is sent as {}: RIE rejects a zero-length body, and a
// client that invokes with no payload is the common case.
func TestAnEmptyPayloadIsSentAsAnEmptyObject(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = readAll(r)
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()
	e := New(Config{Dir: t.TempDir()}, docker.NewFake())
	fn := &Function{ResourceID: "arn:fn", Name: "n", Container: "c", Port: portOf(t, srv.URL)}
	if _, err := e.Invoke(context.Background(), fn, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Errorf("an empty payload was sent as %q", got)
	}
}

// --- helpers --------------------------------------------------------------

func zipWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipExec(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	// The header's external attributes are where the mode lives.
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the entry with an executable mode.
	var exec bytes.Buffer
	zw2 := zip.NewWriter(&exec)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0o755)
	w2, err := zw2.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw2.Close(); err != nil {
		t.Fatal(err)
	}
	return exec.Bytes()
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(url, "http://"), ":")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}
