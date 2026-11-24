package gcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Turning a source archive into a running container.
//
// This is the half that differs from the other two clouds. Lambda MOUNTS the
// code into a vendor image that already contains the runtime interface
// client; Azure Functions must BAKE it in because its .NET host finds nothing
// in a bind mount; and here the runtime contract is a LIBRARY the user's code
// brings, so all Nimbus supplies is a base interpreter and a command.
//
// Nothing reaches the network at deploy time. The base image is pulled by the
// engine before the build, and the Functions Framework comes from the source
// archive -- which is exactly how the azure-functions example carries
// `azure-functions`, by a documented `pip install --target` the user runs
// once. Requiring it is not a shortcut: real Cloud Functions installs
// requirements.txt during a remote build, and a remote build is the one thing
// an offline emulator cannot have.

// runtimes maps a Cloud Functions runtime to the base image that runs it.
//
// Deliberately short. A runtime Nimbus has not been measured against is
// REFUSED and names itself, rather than being served by whichever
// interpreter looks closest -- a python312 function quietly running on 3.11
// would behave almost right, which is worse than not running.
//
// All of these publish linux/arm64, which is why this milestone could follow
// Milestone 21: Azure's Functions host is amd64-only and cannot run on an
// Apple Silicon Mac at all.
var runtimes = map[string]string{
	"python310": "python:3.10-slim",
	"python311": "python:3.11-slim",
	"python312": "python:3.12-slim",
	"python313": "python:3.13-slim",
}

func runtimeImage(runtime string) (string, error) {
	if runtime == "" {
		return "", fmt.Errorf("buildConfig.runtime is required")
	}
	if img, ok := runtimes[strings.ToLower(runtime)]; ok {
		return img, nil
	}
	return "", fmt.Errorf("runtime %q is not implemented; Nimbus runs %s",
		runtime, strings.Join(sortedRuntimes(), ", "))
}

func sortedRuntimes() []string {
	out := make([]string, 0, len(runtimes))
	for k := range runtimes {
		out = append(out, k)
	}
	// Sorted so the refusal reads the same every time.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// functionCodeDir is where a function's unpacked source lives.
//
// An ABSOLUTE path, always. The Azure function app's deploy directory was
// filepath.Join(DataDir, ...) and DataDir is empty without --persist, so in
// the ordinary memory-only mode it came out relative and `docker build` ran
// against the process's working directory -- which, run from the repo,
// committed 157 files of vendored packages into git.
func (s *Server) functionCodeDir(id string) (string, error) {
	base := s.cfg.DataDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "nimbus-gcf")
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("could not resolve the function code directory: %w", err)
	}
	dir := filepath.Join(abs, "functions", slugOf(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("could not create the function code directory: %w", err)
	}
	return dir, nil
}

func (s *Server) removeFunctionCode(id string) {
	dir, err := s.functionCodeDir(id)
	if err != nil {
		return
	}
	_ = os.RemoveAll(dir)
}

// functionImageTag is the image a function's build produces.
//
// Derived from the function rather than remembered, because the engine has to
// be able to REMOVE it after a restart, when nothing remembers having built
// it. An image left behind is the quietest leak there is -- nothing in
// `docker ps` shows it, and Azure Functions leaked 3GB that way.
func functionImageTag(name, id string) string {
	// Lowercase and no slash: a Docker tag may contain neither an uppercase
	// letter nor a `/` in this position, and the function's own name is
	// already lowercase because Cloud Functions requires it.
	return "nimbus-gcf-" + strings.ToLower(name) + "-" + slugOf(id) + ":latest"
}

// writeFunctionDockerfile bakes the source in and runs the framework.
//
// The command is `functions-framework`, which is an ordinary console script
// the package installs -- so this is the user's own dependency doing the
// serving, not Nimbus pretending to be Google. If the archive did not carry
// it, the container exits and the engine reports what the build or the start
// actually said, which is the honest failure.
func writeFunctionDockerfile(dir, base, entryPoint string) error {
	// PYTHONUNBUFFERED so a handler's print() reaches the container console
	// immediately. Without it the output sits in a pipe buffer and `gcloud
	// functions logs`/`docker logs` show nothing until the process exits --
	// which for a server is never.
	// `python -m functions_framework`, NOT the `functions-framework`
	// console script, and the reason is measured rather than stylistic.
	//
	// `pip install --target` writes a console script whose SHEBANG is the
	// absolute path of the interpreter that ran pip -- on the machine that
	// ran it. Vendored from a Mac it reads
	// `#!/opt/homebrew/opt/python@3.14/bin/python3.14`, which does not exist
	// in a linux python:3.11 container, so the container exits with
	// `exec /workspace/bin/functions-framework: no such file or directory`.
	// That message names the script, which DOES exist, and not the
	// interpreter, which does not -- so it reads as a missing dependency and
	// is a missing interpreter. Any vendored console script has this
	// problem, so none can be used.
	//
	// The package ships a __main__.py, so `-m` runs it with the CONTAINER's
	// own Python and the archive only has to be importable. PYTHONPATH is
	// /workspace because that is where the archive was unpacked, which is
	// also where its vendored packages are -- the same arrangement
	// azure-functions uses with .python_packages.
	//
	// PYTHONUNBUFFERED so a handler's print() reaches the container console
	// immediately. Without it the output sits in a pipe buffer and nothing
	// appears until the process exits, which for a server is never.
	content := fmt.Sprintf(`FROM %s
WORKDIR /workspace
COPY . /workspace
ENV PYTHONUNBUFFERED=1
ENV PYTHONPATH=/workspace
ENV FUNCTION_TARGET=%s
EXPOSE %d
CMD ["python", "-m", "functions_framework", "--target=%s", "--port=%d"]
`, base, entryPoint, functionPort, entryPoint, functionPort)
	return os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(content), 0o644)
}
