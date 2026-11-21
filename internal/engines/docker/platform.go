package docker

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// An image with no build for this machine's architecture.
//
// Every image Nimbus pulls is multi-arch except ONE:
// `mcr.microsoft.com/azure-functions/python` is published for linux/amd64
// alone. Measured against the registry on 2026-11-21 -- Lambda's
// `public.ecr.aws/lambda/*`, `rancher/k3s`, `nginx`, `debian` and
// `registry` all carry linux/arm64, and the Functions base does not. So
// on an Apple Silicon Mac the apply dies at the pull with docker's own
// message, which names no image Nimbus could be asked about:
//
//	no matching manifest for linux/arm64/v8 in the manifest list entries
//
// Milestone 20 built Azure Functions on Windows (amd64), and every
// Docker-backed CI job is ubuntu-latest (amd64), so nothing had ever
// pulled it on arm64. That is this project's recurring shape: right on
// the machine it was written on and wrong where nobody looked.
//
// The response is to RETRY under emulation rather than to probe the
// registry first. A probe would cost a network round trip on every pull
// to answer a question that is almost always "yes, natively" -- and it
// would need its own notion of what this host's architecture is, which
// docker already knows. Falling back only after docker itself has said
// the native image does not exist is both cheaper and self-correcting:
// the day Microsoft publishes an arm64 build, the native pull succeeds
// and nothing here runs at all.

// EmulatedPlatform is the one platform Nimbus falls back to. Named
// rather than taken from whatever is available, because an emulated
// container is a real cost and should be an explicit, single choice.
const EmulatedPlatform = "linux/amd64"

// noNativeImage reports docker's "this image has no build for your
// architecture" failure, and ONLY that one.
//
// Narrow on purpose. Retrying any other pull failure under emulation
// would turn one clear error into two confusing ones -- a missing
// repository, a full disk and an unreachable daemon all fail again,
// differently, and the second message is the one the user would see.
func noNativeImage(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no matching manifest") ||
		strings.Contains(msg, "no match for platform in manifest")
}

// platforms remembers which images had to be emulated.
//
// It lives on the CLI rather than being passed through every caller
// because the knowledge is discovered in ONE place (the pull) and needed
// in two others (the build and the run) that have no reason to care. A
// per-caller parameter would be a per-engine opt-in, which is exactly
// what left `--add-host` missing from the EKS and GKE paths for three
// milestones.
type platforms struct {
	mu       sync.Mutex
	emulated map[string]bool
}

func (p *platforms) note(image string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.emulated == nil {
		p.emulated = map[string]bool{}
	}
	p.emulated[image] = true
}

func (p *platforms) isEmulated(image string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emulated[image]
}

// Platform reports the platform an image must be run with, or "" when it
// runs natively. Exported so a caller can SAY an app is emulated rather
// than leaving someone to wonder why it is slow.
func (c *CLI) Platform(image string) string {
	if c.platforms.isEmulated(image) {
		return EmulatedPlatform
	}
	return ""
}

// pullArgs builds the pull command.
func pullArgs(image string, emulated bool) []string {
	if emulated {
		return []string{"pull", "--platform", EmulatedPlatform, image}
	}
	return []string{"pull", image}
}

// baseImageOf reads the FROM line out of a generated Dockerfile.
//
// A build inherits its base's architecture, so a build FROM an emulated
// image must itself be emulated -- otherwise docker picks the host's and
// the COPY lands on a base that cannot execute, failing later with `exec
// format error`, which names neither the image nor the architecture.
//
// Only the first FROM is read, and that is enough: Nimbus GENERATES the
// only Dockerfile it ever builds (writeFunctionDockerfile), and it has
// exactly one stage. A hand-written multi-stage file is not something
// this path can receive -- an unpack overwrites it every time.
func baseImageOf(contextDir string) string {
	data, err := os.ReadFile(filepath.Join(contextDir, "Dockerfile"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") {
			return fields[1]
		}
	}
	return ""
}
