package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Building an image from a directory.
//
// Nothing in Nimbus needed this before Milestone 20: every engine until
// then either pulls a published image (k3s, Cloud Run, a VM) or
// bind-mounts code into one that already knows how to run it (Lambda,
// through the Runtime Interface Emulator). The Azure Functions host is the
// exception, and it was measured rather than assumed -- with the code
// bind-mounted at /home/site/wwwroot the host reports
//
//	0 functions found (Custom)
//	No functions were found. This can occur before you deploy code...
//
// for BOTH programming models, while the identical tree baked in with a
// COPY loads and serves it. So a function app gets an image of its own.
//
// This stays offline: the base image is already local (pulled like any
// other) and a COPY touches no network. A build with a FROM that is not
// present would reach out, which is why the caller pulls the base first
// rather than letting the build do it.

// buildArgs is the command line, with the context directory LAST.
//
// `docker build` takes the context as a positional argument, so anything
// appended after it is read as a second positional and refused -- the same
// shape as runArgs putting the image and its command last, where a flag
// in the wrong place silently becomes the container's own argument.
//
// The context path is passed as ONE argv element and never through a
// shell, so a Windows path with a drive letter or a space needs no
// quoting and no conversion. That also means the Git Bash path-rewriting
// trap from Milestone 18 cannot apply here.
func buildArgs(tag, contextDir string) []string {
	return []string{
		"build",
		// Quiet: the build log is noise unless it fails, and a failure
		// comes back in stderr either way.
		"-q",
		"-t", tag,
		contextDir,
	}
}

// Build builds an image from a directory containing a Dockerfile.
func (c *CLI) Build(ctx context.Context, contextDir, tag string) error {
	// Combined output, because a build's diagnostics go to stdout as well
	// as stderr and a failure is unreadable with half of it missing.
	if _, err := c.runCombined(ctx, buildArgs(tag, contextDir)...); err != nil {
		return fmt.Errorf("building %s: %w", tag, err)
	}
	return nil
}

// ImageTag is a stable, legal image tag for one resource.
//
// Derived from the resource id rather than generated, for the reason every
// derived value in this project exists: a redeploy must replace its own
// image instead of leaving one behind per deploy, and a delete has to be
// able to work out what to remove.
//
// Lowercased throughout, because Docker refuses an uppercase reference and
// its error -- "invalid reference format" -- names neither the case nor
// the field. Azure makes this a live concern rather than a theoretical
// one: ARM answers with the casing used at creation, so a resource id and
// a resource name are both routinely mixed case. The id is folded before
// hashing too, since ARM resolves ids case-insensitively and the same
// resource must not get two images.
func ImageTag(prefix, resourceID, name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(resourceID)))
	return fmt.Sprintf("%s-%s-%s:latest",
		SanitiseName(prefix), SanitiseName(name), hex.EncodeToString(sum[:4]))
}

// RemoveImage removes an image, ignoring one that is already gone.
//
// Only a BUILT image is ever removed. A pulled one is shared with
// everything else on the machine -- removing nginx:alpine because one
// Container App used it would break every other app and cost a re-pull --
// while an image built for a single resource is that resource's own, and
// would otherwise accumulate one per app, invisibly, at roughly a
// gigabyte each.
func (c *CLI) RemoveImage(ctx context.Context, tag string) error {
	_, err := c.run(ctx, "image", "rm", "-f", tag)
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return nil
	}
	return err
}
