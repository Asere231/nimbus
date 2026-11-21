package docker

import (
	"errors"
	"strings"
	"testing"
)

// An image with no build for this machine's architecture.
//
// Every image Nimbus pulls is multi-arch except ONE:
// `mcr.microsoft.com/azure-functions/python` is published for linux/amd64
// alone. On an Apple Silicon Mac -- which is most Macs now -- the apply
// dies at the pull with docker's own message and nothing about Nimbus:
//
//	no matching manifest for linux/arm64/v8 in the manifest list entries
//
// Milestone 20 built Azure Functions on Windows (amd64) and every
// Docker-backed CI job is ubuntu-latest (amd64), so nothing had ever
// pulled it on arm64. This is the Milestone 21 shape exactly: right on
// the machine it was written on, broken where nobody looked.

func TestAMissingNativeManifestIsRecognised(t *testing.T) {
	// Docker's wording, verbatim from a real failed pull on arm64.
	real := errors.New(`no matching manifest for linux/arm64/v8 in the manifest list entries: ` +
		`no match for platform in manifest: not found`)
	if !noNativeImage(real) {
		t.Error("the real arm64 manifest error was not recognised")
	}

	// Not every pull failure is this one, and retrying the others under
	// emulation would turn one clear error into two confusing ones.
	for _, other := range []error{
		errors.New("manifest unknown"),
		errors.New("pull access denied, repository does not exist"),
		errors.New("no space left on device"),
		errors.New("Cannot connect to the Docker daemon"),
		nil,
	} {
		if noNativeImage(other) {
			t.Errorf("%v was mistaken for a missing-architecture error", other)
		}
	}
}

// The fallback is one platform, named, rather than whatever is handy: an
// emulated image is a real cost, so it is taken only when the native pull
// has already failed for exactly this reason.
func TestTheEmulatedPlatformIsLinuxAmd64(t *testing.T) {
	if EmulatedPlatform != "linux/amd64" {
		t.Errorf("EmulatedPlatform = %q, want linux/amd64", EmulatedPlatform)
	}
	args := pullArgs("some/image:tag", true)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--platform linux/amd64") {
		t.Errorf("an emulated pull does not request the platform: %v", args)
	}
	if strings.Contains(strings.Join(pullArgs("some/image:tag", false), " "), "--platform") {
		t.Error("a native pull must not request a platform at all")
	}
}

// A build FROM an emulated base has to be emulated too, or docker picks
// the host's architecture for the new image and the COPY lands on a base
// that cannot run. Azure Functions is the only caller that builds, and it
// is the only image that needs this -- so the two meet in one place.
func TestAnEmulatedBuildRequestsThePlatform(t *testing.T) {
	joined := strings.Join(buildArgs("t:latest", "/ctx", true), " ")
	if !strings.Contains(joined, "--platform linux/amd64") {
		t.Errorf("an emulated build does not request the platform: %s", joined)
	}
	if strings.Contains(strings.Join(buildArgs("t:latest", "/ctx", false), " "), "--platform") {
		t.Error("an ordinary build must not request a platform")
	}
}

// Running the image has to match how it was pulled. A container started
// without the flag against an emulated image fails with `exec format
// error`, which names neither the architecture nor the image.
func TestAnEmulatedRunRequestsThePlatform(t *testing.T) {
	native := runArgs(RunOptions{Name: "x", Image: "i"})
	emulated := runArgs(RunOptions{Name: "x", Image: "i", Platform: EmulatedPlatform})

	if strings.Contains(strings.Join(native, " "), "--platform") {
		t.Error("an ordinary run must not request a platform")
	}
	if !strings.Contains(strings.Join(emulated, " "), "--platform linux/amd64") {
		t.Errorf("an emulated run does not request the platform: %v", emulated)
	}
}

// The memo cannot be the mechanism, because the thing that fills it is
// skipped on every run but the first.
//
// Pull is only called when the image is NOT already local, so after one
// successful deploy -- or any restart -- nothing records that the base
// needs emulating, and a build that trusted the memo chose the host's
// architecture and failed on the Dockerfile's FROM. Measured: the first
// apply on Apple Silicon succeeded and the second failed at
// `Dockerfile:3`. The retry is what makes it stateless.
func TestABuildFallsBackWithoutHavingPulledFirst(t *testing.T) {
	var fresh platforms
	if fresh.isEmulated("mcr.microsoft.com/azure-functions/python:4-python3.11") {
		t.Fatal("a fresh process must know nothing; that is the point")
	}
	// The fallback decision comes from the ERROR, not from the memo, so
	// a process that never pulled still recovers.
	err := errors.New("no matching manifest for linux/arm64/v8 in the manifest list entries")
	if !noNativeImage(err) {
		t.Error("the build's retry condition does not recognise the real error")
	}
}

// A failed build must report the CAUSE, which BuildKit prints last.
//
// `runCombined` kept only the first line, so every failed build said
// `Dockerfile:3` and nothing else -- and Build's retry, which matches on
// the text of the missing-architecture error, never saw it. One helper
// quietly defeating its only caller's stated purpose.
func TestAFailedBuildReportsTheCauseNotTheFirstLine(t *testing.T) {
	// Real BuildKit output, abridged: excerpt first, cause last.
	out := `#0 building with "desktop-linux" instance
Dockerfile:3
--------------------
   2 |     # c
   3 | >>> FROM mcr.microsoft.com/azure-functions/python:4-python3.11
   4 |     ENV X=1
--------------------
ERROR: failed to build: failed to solve: mcr.microsoft.com/azure-functions/python:4-python3.11: no match for platform in manifest: not found`

	got := diagnosis(out)
	if !strings.Contains(got, "no match for platform") {
		t.Errorf("the diagnosis does not name the cause: %q", got)
	}
	if !noNativeImage(errors.New(got)) {
		t.Error("the diagnosis is not recognisable as a missing-architecture error, so the retry would not fire")
	}
	// A command whose output has no ERROR line still reports something.
	if diagnosis("Cannot connect to the Docker daemon\nis the daemon running?") == "" {
		t.Error("a non-build failure lost its message")
	}
	if diagnosis("") == "" {
		t.Error("empty output must still produce a message")
	}
}
