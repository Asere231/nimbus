package gcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Dockerfile must run the module, NOT the console script.
//
// This is the finding that cost the most to reach, and it is invisible until
// a container actually starts. `pip install --target` writes a console script
// whose SHEBANG is the absolute path of the interpreter that ran pip, on the
// machine that ran it -- vendored from a Mac it reads
// `#!/opt/homebrew/opt/python@3.14/bin/python3.14`, which does not exist in a
// linux python:3.11 container.
//
// The container then exits with
//
//	exec /workspace/bin/functions-framework: no such file or directory
//
// which names the script, and the script DOES exist. What is missing is the
// interpreter in its first line, so the message reads as a missing dependency
// and is a missing interpreter. Every vendored console script has this
// problem, so none can be used: `python -m` runs the package with the
// container's own Python instead.
func TestTheFunctionDockerfileRunsTheModuleNotTheConsoleScript(t *testing.T) {
	dir := t.TempDir()
	if err := writeFunctionDockerfile(dir, "python:3.11-slim", "hello"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)

	if !strings.Contains(content, `CMD ["python", "-m", "functions_framework"`) {
		t.Errorf("the CMD does not run the module with the container's own Python:\n%s", content)
	}
	// The console script must not be invoked by name anywhere, because a
	// vendored one carries the vendoring machine's interpreter path.
	if strings.Contains(content, `"functions-framework"`) || strings.Contains(content, "/bin/functions-framework") {
		t.Errorf("the Dockerfile invokes the console script, whose shebang points at another machine:\n%s", content)
	}
	for _, want := range []string{
		"FROM python:3.11-slim",
		"--target=hello",
		"PYTHONPATH=/workspace", // where the archive's vendored packages are
		"PYTHONUNBUFFERED=1",    // or a handler's print() never appears
	} {
		if !strings.Contains(content, want) {
			t.Errorf("the Dockerfile is missing %q:\n%s", want, content)
		}
	}
}

// A derived value that becomes a NAME must be a legal one.
//
// etagOf is base64, so it contains uppercase letters, `/` and `=`. Used as a
// Docker tag it is refused outright -- `docker build` says "repository name
// must be lowercase", which reads as a complaint about the name Nimbus chose
// rather than about the encoding it chose -- and used as a directory the
// slash silently turned one directory into two nested ones. Both measured on
// the first real deploy.
func TestAFunctionsImageTagIsALegalDockerTag(t *testing.T) {
	for _, name := range []string{"hello-fn", "nimbus-hello", "a"} {
		tag := functionImageTag(name, "projects/p/locations/l/functions/"+name)
		if tag != strings.ToLower(tag) {
			t.Errorf("tag %q has uppercase letters; docker refuses them", tag)
		}
		// A tag is repository:tag, so exactly one colon and no slash in the
		// repository part.
		repo, version, ok := strings.Cut(tag, ":")
		if !ok || version != "latest" {
			t.Errorf("tag %q is not repository:latest", tag)
		}
		if strings.ContainsAny(repo, "/+= ") {
			t.Errorf("tag %q contains a character docker will not take in this position", tag)
		}
	}
	// Two functions get two tags, or a deploy would remove the other's image.
	a := functionImageTag("one", "projects/p/locations/l/functions/one")
	b := functionImageTag("two", "projects/p/locations/l/functions/two")
	if a == b {
		t.Error("two functions share an image tag")
	}
}

// slugOf is hex, and stable, because it names a directory and an image.
func TestSlugOfIsStableLowercaseHex(t *testing.T) {
	const seed = "projects/p/locations/l/functions/f"
	first := slugOf(seed)
	if first != slugOf(seed) {
		t.Error("slugOf is not stable; the image tag would change between deploys")
	}
	if first == "" {
		t.Fatal("slugOf returned nothing")
	}
	for _, c := range first {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("slugOf returned %q, which is not lowercase hex", first)
			break
		}
	}
	if slugOf(seed) == slugOf(seed+"x") {
		t.Error("slugOf collides on different seeds")
	}
}

// Every runtime in the table is a real image, and an unknown one is refused
// naming the alternatives.
func TestRuntimeImageRefusesWhatItCannotRun(t *testing.T) {
	if img, err := runtimeImage("python311"); err != nil || img != "python:3.11-slim" {
		t.Errorf("python311 -> %q, %v", img, err)
	}
	// Case-insensitive, because a client may send either.
	if _, err := runtimeImage("PYTHON311"); err != nil {
		t.Errorf("an upper-case runtime was refused: %v", err)
	}
	_, err := runtimeImage("nodejs20")
	if err == nil {
		t.Fatal("a runtime Nimbus does not run was accepted")
	}
	// The refusal lists what IS available, so the next step is obvious.
	if !strings.Contains(err.Error(), "nodejs20") || !strings.Contains(err.Error(), "python311") {
		t.Errorf("the refusal does not name the runtime and the alternatives: %v", err)
	}
}

// The code directory is ABSOLUTE even with no data directory.
//
// The Azure function app's was filepath.Join(DataDir, ...) and DataDir is
// empty without --persist, so in the ordinary memory-only mode it came out
// relative and `docker build` ran against the process's working directory --
// which, run from the repo, committed 157 files of vendored packages to git.
func TestTheFunctionCodeDirectoryIsAbsolute(t *testing.T) {
	s := newTestServer(t)
	if s.cfg.DataDir != "" {
		t.Fatal("this test is about the memory-only case and DataDir is set")
	}
	dir, err := s.functionCodeDir("projects/p/locations/l/functions/f")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("the code directory %q is relative; docker build would run against the cwd", dir)
	}
	// And it is one directory, not a nested pair -- which is what a base64
	// name produced, because base64 contains "/".
	rel := strings.TrimPrefix(dir, filepath.Join(os.TempDir(), "nimbus-gcf", "functions")+string(filepath.Separator))
	if strings.ContainsRune(rel, filepath.Separator) {
		t.Errorf("the code directory name %q contains a separator", rel)
	}
	t.Cleanup(func() { s.removeFunctionCode("projects/p/locations/l/functions/f") })
}
