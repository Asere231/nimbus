package docker

import (
	"context"
	"strings"
	"testing"
)

// Building an image, which is new in Milestone 20.
//
// Nothing needed it before: every engine until now either pulls a
// published image or bind-mounts code into one. The Azure Functions host
// is the exception, measured rather than assumed -- with the code
// bind-mounted at /home/site/wwwroot the host reports "0 functions found"
// for both programming models, and the identical tree baked in with a
// COPY loads and serves it. So a function app needs an image of its own,
// built from a local base image plus a COPY, which touches no network.

// The context directory comes LAST, after the flags.
//
// `docker build` takes the context as a positional argument, so anything
// appended after it is read as a second positional and refused -- the same
// trap TestTheImageAndItsArgumentsComeLast pins for `docker run`, where a
// flag after the image silently becomes the container's own argument.
func TestTheBuildContextComesLast(t *testing.T) {
	args := buildArgs("nimbus-fnapp-web-abc12345:latest", "/tmp/code")

	if args[0] != "build" {
		t.Fatalf("not a build: %v", args)
	}
	if got := args[len(args)-1]; got != "/tmp/code" {
		t.Errorf("the context is %q, not the last argument: %v", got, args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-t nimbus-fnapp-web-abc12345:latest") {
		t.Errorf("the tag is missing from %v", args)
	}
}

// A Windows context path is passed through unchanged.
//
// It goes to exec.Command as ONE argv element, so there is no shell to
// mangle a drive letter or a space, and no conversion to attempt -- the
// Docker CLI on Windows takes `C:\Users\...` as a context directory. The
// Git Bash path-rewriting trap that cost Milestone 18 three measurements
// does not apply, because Nimbus never goes through a shell.
func TestAWindowsContextPathIsNotRewritten(t *testing.T) {
	const win = `C:\Users\Someone\AppData\Local\Temp\nimbus fn\code`
	args := buildArgs("tag:latest", win)
	if got := args[len(args)-1]; got != win {
		t.Errorf("the context path was altered: %q", got)
	}
}

// An image tag is lowercase, because Docker refuses anything else.
//
// Azure resource ids and names are mixed case -- ARM answers with the
// casing used at creation, which CLAUDE.md records as load-bearing -- so a
// tag derived from one has to be folded. Docker's error for an uppercase
// tag ("invalid reference format") names neither the case nor the field.
func TestAnImageTagIsAlwaysLegal(t *testing.T) {
	cases := []string{
		"/subscriptions/0000/resourceGroups/RG-Nimbus/providers/Microsoft.Web/sites/MyFuncApp",
		"/subscriptions/0000/resourcegroups/rg/providers/microsoft.web/sites/app",
	}
	seen := map[string]bool{}
	for _, id := range cases {
		tag := ImageTag("nimbus-fnapp", id, "MyFuncApp")
		if tag != strings.ToLower(tag) {
			t.Errorf("tag %q is not lowercase; docker refuses that", tag)
		}
		if !strings.Contains(tag, ":") {
			t.Errorf("tag %q has no tag part", tag)
		}
		repo := tag[:strings.Index(tag, ":")]
		for _, r := range repo {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				t.Errorf("tag %q has %q in its repository part, which docker refuses", tag, r)
			}
		}
		seen[tag] = true
	}
	// Two different ids must not collide, or deploying one app would
	// overwrite another's image.
	if len(seen) != len(cases) {
		t.Errorf("two different resource ids produced the same tag: %v", seen)
	}
}

// The same resource always produces the same tag, so a redeploy replaces
// its own image rather than accumulating one per deploy.
func TestAnImageTagIsStableForOneResource(t *testing.T) {
	const id = "/subscriptions/0000/resourceGroups/rg/providers/Microsoft.Web/sites/app"
	first := ImageTag("nimbus-fnapp", id, "app")
	second := ImageTag("nimbus-fnapp", id, "app")
	if first != second {
		t.Errorf("the tag is not stable: %q then %q", first, second)
	}
	// Case-insensitively the same id is the same resource, which is how
	// ARM resolves ids -- so it must be the same image.
	upper := ImageTag("nimbus-fnapp", strings.ToUpper(id), "app")
	if upper != first {
		t.Errorf("the same id in different casing gave %q and %q; ARM treats them as one resource",
			upper, first)
	}
}

// The Fake records the build and marks the image present, so a caller
// that builds and then asks whether to pull gets the right answer.
//
// Without that, every engine's start path would try to PULL an image that
// exists only locally, and the pull would fail against a registry that has
// never heard of it.
func TestTheFakeRecordsABuildAndMarksTheImagePresent(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	if present, _ := f.ImagePresent(ctx, "nimbus-fnapp-web-abc:latest"); present {
		t.Fatal("the image should not be present before the build")
	}
	if err := f.Build(ctx, "/tmp/code", "nimbus-fnapp-web-abc:latest"); err != nil {
		t.Fatal(err)
	}
	if present, _ := f.ImagePresent(ctx, "nimbus-fnapp-web-abc:latest"); !present {
		t.Error("a built image must read as present, or the caller tries to pull it")
	}

	dir, tag, ok := f.BuiltImage("nimbus-fnapp-web-abc:latest")
	if !ok {
		t.Fatalf("the build was not recorded; calls: %v", f.Calls)
	}
	if dir != "/tmp/code" || tag != "nimbus-fnapp-web-abc:latest" {
		t.Errorf("recorded the wrong build: dir=%q tag=%q", dir, tag)
	}
}

// An unavailable daemon fails the build the same way it fails everything
// else, so a caller gets ErrUnavailable and can say "start Docker".
func TestABuildFailsWhenTheDaemonIsUnavailable(t *testing.T) {
	f := NewFake()
	f.Unavailable = true
	if err := f.Build(context.Background(), "/tmp/code", "tag:latest"); err == nil {
		t.Error("a build against a dead daemon should fail")
	}
}

// FailBuild lets a test put a deploy in the state where the image could
// not be built, which is a real outcome -- a syntax error in a generated
// Dockerfile, a base image that is not present -- and must surface as a
// failed resource rather than a container that never appears.
func TestFailBuildIsHonoured(t *testing.T) {
	f := NewFake()
	f.FailBuild = ErrUnavailable
	if err := f.Build(context.Background(), "/tmp/code", "tag:latest"); err == nil {
		t.Error("FailBuild was ignored")
	}
}
