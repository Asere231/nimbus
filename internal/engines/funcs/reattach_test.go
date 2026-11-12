package funcs

import (
	"context"
	"testing"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A function's container outlives Nimbus with --persist, so a fresh engine
// has to ADOPT it rather than forget it.
//
// Forgetting it is what used to happen, and the symptom was worse than an
// orphan: the store still described the function, so a client read it as
// Active, while `Get` returned nil and every invoke answered
// ResourceNotReadyException -- with the container plainly Up in `docker ps`.
// Nothing in that picture points at the cause.
//
// Everything needed is on the container: the resource id and the name are
// labels, and the port RIE answers on is the published one. The port
// matters most -- an adopted function with no port is one that cannot be
// invoked, which is the whole reason for adopting it.
func TestReattachAdoptsFunctionContainers(t *testing.T) {
	f := docker.NewFake()
	ctx := context.Background()

	// A container of the shape Start leaves behind.
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-lambda-writer-2890b176",
		Image: "public.ecr.aws/lambda/python:3.12",
		Ports: map[string]string{"127.0.0.1:9123": "8080"},
		Labels: map[string]string{
			docker.LabelFunction: "writer",
			docker.LabelResource: "arn:aws:lambda:us-east-1:000000000000:function:writer",
			docker.LabelRole:     "function",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// A completely fresh engine over the same daemon: this is a restart.
	e := New(Config{NamePrefix: "nimbus-lambda", Dir: t.TempDir()}, f)
	found := e.Reattach(ctx, func(string) bool { return true })

	if len(found) != 1 {
		t.Fatalf("adopted %d functions, want 1: %v", len(found), found)
	}
	fn := e.Get("arn:aws:lambda:us-east-1:000000000000:function:writer")
	if fn == nil {
		t.Fatal("the function is not in the registry after reattaching")
	}
	if fn.Name != "writer" {
		t.Errorf("name = %q, want writer", fn.Name)
	}
	if fn.Container != "nimbus-lambda-writer-2890b176" {
		t.Errorf("container = %q", fn.Container)
	}
	// Without the port an adopted function cannot be invoked, which would
	// make adopting it pointless.
	if fn.Port != 9123 {
		t.Errorf("port = %d, want 9123 (recovered from the published port)", fn.Port)
	}
}

// A container whose resource is gone is left alone.
//
// `keep` is how a plugin recognises its own: a resource id is already
// cloud-shaped, so asking the store whether it still exists is both simpler
// and surer than matching a container-name prefix. Same division k3s and vm
// use.
func TestReattachLeavesFunctionsWhoseResourceIsGone(t *testing.T) {
	f := docker.NewFake()
	ctx := context.Background()
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-lambda-ghost-1",
		Image: "public.ecr.aws/lambda/python:3.12",
		Ports: map[string]string{"127.0.0.1:9124": "8080"},
		Labels: map[string]string{
			docker.LabelFunction: "ghost",
			docker.LabelResource: "arn:aws:lambda:us-east-1:000000000000:function:ghost",
			docker.LabelRole:     "function",
		},
	}); err != nil {
		t.Fatal(err)
	}

	e := New(Config{NamePrefix: "nimbus-lambda", Dir: t.TempDir()}, f)
	if found := e.Reattach(ctx, func(string) bool { return false }); len(found) != 0 {
		t.Errorf("adopted %d functions whose resource is gone, want 0", len(found))
	}
	if e.Get("arn:aws:lambda:us-east-1:000000000000:function:ghost") != nil {
		t.Error("a function whose resource is gone was put in the registry")
	}
}
