package aws

import (
	"context"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A restart with --persist adopts EVERY engine's containers, not just k3s.
//
// This is the test the gap it closes never had. All three clouds reattached
// `s.k3s` and nothing else -- vm.Engine.Reattach was written and unit-tested
// and called by NOBODY, and funcs and run had no Reattach at all. A unit
// test proving a method works while nothing invokes it is this repo's
// "passes for the wrong reason" trap in its purest form, so the assertion
// that matters is made HERE, at the cloud, where the wiring lives.
//
// What it cost: with --persist the containers outlive the process by
// design, so after a restart the store still described a Lambda as Active
// while every invoke answered ResourceNotReadyException, and a destroy
// could not find the container to remove. The container sat plainly Up in
// `docker ps`.
//
// reattach is called synchronously rather than through Start's goroutine,
// so this asserts a state the test created instead of racing a background
// adoption -- the lesson the AKS credentials test and the VM provisioning
// test both wrote down.
func TestARestartAdoptsEveryEnginesContainers(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		PublicURL: "http://localhost:4702", DataDir: t.TempDir(),
		ClusterTimeout: 50 * time.Millisecond,
		KeepClusters:   true, // what --persist sets
	}, store, bus)

	f := docker.NewFake()
	s.UseDocker(f)
	ctx := context.Background()

	// Two resources a previous Nimbus created, still in the store...
	const fnARN = "arn:aws:lambda:us-east-1:000000000000:function:writer"
	const instanceID = "i-0123456789abcdef0"
	store.Put(&core.Resource{ID: fnARN, Cloud: "aws", Type: "AWS::Lambda::Function", Name: "writer"})
	store.Put(&core.Resource{ID: instanceID, Cloud: "aws", Type: "AWS::EC2::Instance", Name: "web"})

	// ...and the containers they left running.
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-lambda-writer-2890b176",
		Image: "public.ecr.aws/lambda/python:3.12",
		Ports: map[string]string{"127.0.0.1:9123": "8080"},
		Labels: map[string]string{
			docker.LabelFunction: "writer", docker.LabelResource: fnARN,
			docker.LabelRole: "function",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, docker.RunOptions{
		Name:  "nimbus-ec2-web-4d6415ed",
		Image: "debian:12",
		Labels: map[string]string{
			docker.LabelInstance: "web", docker.LabelResource: instanceID,
			docker.LabelRole: "instance",
		},
	}); err != nil {
		t.Fatal(err)
	}

	s.reattach(ctx)

	fn := s.lambda.Get(fnARN)
	if fn == nil {
		t.Error("the Lambda was not adopted; every invoke would answer ResourceNotReadyException")
	} else if fn.Port != 9123 {
		t.Errorf("the Lambda was adopted without its port (%d), so it cannot be invoked", fn.Port)
	}

	if m := s.vms.Get(instanceID); m == nil {
		t.Error("the instance was not adopted; a terminate could not remove its container")
	} else if m.Container != "nimbus-ec2-web-4d6415ed" {
		t.Errorf("instance container = %q", m.Container)
	}
}

// A container whose resource is gone is left alone, on every engine. The
// store is the authority on what is still wanted, which is also how two
// clouds sharing an engine tell their containers apart.
func TestARestartLeavesContainersWhoseResourceIsGone(t *testing.T) {
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		PublicURL: "http://localhost:4702", DataDir: t.TempDir(),
		ClusterTimeout: 50 * time.Millisecond, KeepClusters: true,
	}, store, bus)
	f := docker.NewFake()
	s.UseDocker(f)
	ctx := context.Background()

	// A container with nothing in the store to match it.
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

	s.reattach(ctx)

	if s.lambda.Get("arn:aws:lambda:us-east-1:000000000000:function:ghost") != nil {
		t.Error("adopted a function whose resource no longer exists")
	}
}
