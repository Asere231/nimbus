package k3s

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Fake PEM blobs, enough for the certificate-reading half of waitReady.
const (
	fakeCA   = "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"
	fakeCert = "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n"
	fakeKey  = "-----BEGIN EC PRIVATE KEY-----\nkey\n-----END EC PRIVATE KEY-----\n"
)

// A cloud's own k3s arguments have to reach the container, and so do the
// files they name.
//
// This is what makes EKS authentication work: the token webhook is passed as
// a --kube-apiserver-arg and its kubeconfig is bind-mounted, because k3s
// reads its arguments at startup and there is no moment between the
// container existing and needing the file. An argument that silently went
// nowhere would show up much later as `kubectl` getting a 401 it cannot
// explain.
func TestAServerArgAndItsFileReachTheContainer(t *testing.T) {
	bus := core.NewBus(10)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	f := docker.NewFake()
	f.Images["rancher/k3s:test"] = true
	f.ExecOutput["cat "+AdminCertPath] = []byte(fakeCert)
	f.ExecOutput["cat "+AdminKeyPath] = []byte(fakeKey)
	f.ExecOutput["cat "+ServerCAPath] = []byte(fakeCA)

	e := New(Config{
		Image:   "rancher/k3s:test",
		Version: "1.34",
		// The readiness probe talks to a real port, which no fake can
		// provide, so this run is expected to time out. What is under test
		// is the container it asked Docker for.
		Timeout:       50 * time.Millisecond,
		PortBase:      6700,
		NamePrefix:    "nimbus-test",
		ServerArgs:    []string{"--kube-apiserver-arg=--authentication-token-webhook-config-file=/etc/rancher/k3s/w.yaml"},
		ServerVolumes: map[string]string{"/host/w.yaml": "/etc/rancher/k3s/w.yaml"},
	}, f, store, "aws")

	cl, err := e.NewCluster("arn:aws:eks:us-east-1:000000000000:cluster/nimbus", "nimbus")
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Start(context.Background(), cl) // times out on the probe, by design

	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, "authentication-token-webhook-config-file=/etc/rancher/k3s/w.yaml") {
		t.Errorf("the server argument never reached Docker:\n%s", calls)
	}
	if !strings.Contains(calls, "/host/w.yaml:/etc/rancher/k3s/w.yaml") {
		t.Errorf("the file the argument names was not mounted:\n%s", calls)
	}
	// The arguments a cluster always needs must still be there.
	for _, want := range []string{"server", "--tls-san", "--disable"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing the base argument %q:\n%s", want, calls)
		}
	}
}
