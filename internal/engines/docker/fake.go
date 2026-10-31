package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Fake is an in-memory Client for tests.
//
// It lives beside the real implementation rather than in a _test.go file so
// the azure package can use it too: the whole AKS hook -- provisioning states,
// credential stability, scaling -- is then testable with no daemon, and
// `go test ./...` stays hermetic and fast. The one opt-in integration test
// gated on NIMBUS_DOCKER_TESTS=1 is what exercises real k3s.
type Fake struct {
	mu sync.Mutex

	// ExecAny makes Exec succeed with empty output for commands that have no
	// canned entry. Callers that WRITE inside a container (the registry
	// mirrors) do not read anything back, so insisting on canned output for
	// them would only pin the exact shell line in a test.
	ExecAny bool

	// Unavailable makes every call fail with ErrUnavailable, for testing the
	// "Docker is not running" path -- which must produce a clear error rather
	// than a hang.
	Unavailable bool
	// FailRun, when set, makes Run return this error.
	FailRun error
	// ExecOutput maps a command (joined by spaces) to its stdout, so a test
	// can stand in for reading certificates out of a container.
	ExecOutput map[string][]byte
	// Images that ImagePresent reports as already pulled.
	Images map[string]bool

	containers map[string]*Container
	networks   map[string]bool
	// Calls records every method invoked, in order, for assertions about what
	// was actually done.
	Calls []string
}

// NewFake returns an empty Fake with a running "daemon".
func NewFake() *Fake {
	return &Fake{
		ExecOutput: map[string][]byte{},
		Images:     map[string]bool{},
		containers: map[string]*Container{},
		networks:   map[string]bool{},
	}
}

func (f *Fake) record(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

func (f *Fake) check() error {
	if f.Unavailable {
		return fmt.Errorf("%w: fake daemon is stopped", ErrUnavailable)
	}
	return nil
}

// Available reports the fake daemon's state.
func (f *Fake) Available(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("available")
	return f.check()
}

// Run records a container as started.
func (f *Fake) Run(ctx context.Context, opts RunOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("run %s image=%s args=%v", opts.Name, opts.Image, opts.Args)
	if err := f.check(); err != nil {
		return "", err
	}
	if f.FailRun != nil {
		return "", f.FailRun
	}
	if _, exists := f.containers[opts.Name]; exists {
		return "", fmt.Errorf("docker: container %s already exists", opts.Name)
	}
	ports := map[string]string{}
	for host, container := range opts.Ports {
		ports[container+"/tcp"] = host
	}
	id := fmt.Sprintf("fake%08x", len(f.containers)+1)
	f.containers[opts.Name] = &Container{
		ID: id, Name: opts.Name, Image: opts.Image,
		Running: true, Status: "running",
		Labels: opts.Labels, Ports: ports,
	}
	return id, nil
}

// Stop marks a container stopped.
func (f *Fake) Stop(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop %s", name)
	if err := f.check(); err != nil {
		return err
	}
	if c, ok := f.containers[name]; ok {
		c.Running, c.Status = false, "exited"
	}
	return nil // already gone is not an error, as with the real client
}

// Remove deletes a container.
func (f *Fake) Remove(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove %s", name)
	if err := f.check(); err != nil {
		return err
	}
	delete(f.containers, name)
	return nil
}

// Exec returns canned output for a command.
func (f *Fake) Exec(ctx context.Context, name string, cmd ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(cmd, " ")
	f.record("exec %s %s", name, joined)
	if err := f.check(); err != nil {
		return nil, err
	}
	if _, ok := f.containers[name]; !ok {
		return nil, &NotFoundError{Name: name}
	}
	if out, ok := f.ExecOutput[joined]; ok {
		return out, nil
	}
	if f.ExecAny {
		return nil, nil
	}
	return nil, fmt.Errorf("fake docker: no canned output for %q in %s", joined, name)
}

// Inspect returns a container.
func (f *Fake) Inspect(ctx context.Context, name string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect %s", name)
	if err := f.check(); err != nil {
		return Container{}, err
	}
	c, ok := f.containers[name]
	if !ok {
		return Container{}, &NotFoundError{Name: name}
	}
	return *c, nil
}

// List returns containers carrying a label, sorted by name so tests are
// deterministic.
func (f *Fake) List(ctx context.Context, label string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list %s", label)
	if err := f.check(); err != nil {
		return nil, err
	}
	key, want, exact := strings.Cut(label, "=")
	var out []Container
	for _, c := range f.containers {
		v, ok := c.Labels[key]
		if !ok || (exact && v != want) {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// EnsureNetwork records a network.
func (f *Fake) EnsureNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("ensure-network %s", name)
	if err := f.check(); err != nil {
		return err
	}
	f.networks[name] = true
	return nil
}

// ConnectNetwork records a container joining a network.
func (f *Fake) ConnectNetwork(ctx context.Context, network, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("connect-network %s %s", network, container)
	if err := f.check(); err != nil {
		return err
	}
	f.networks[network] = true
	return nil
}

// RemoveNetwork forgets a network.
func (f *Fake) RemoveNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove-network %s", name)
	if err := f.check(); err != nil {
		return err
	}
	delete(f.networks, name)
	return nil
}

// ImagePresent reports whether the fake has the image.
func (f *Fake) ImagePresent(ctx context.Context, image string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("image-present %s", image)
	if err := f.check(); err != nil {
		return false, err
	}
	return f.Images[image], nil
}

// Pull marks an image as present.
func (f *Fake) Pull(ctx context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("pull %s", image)
	if err := f.check(); err != nil {
		return err
	}
	f.Images[image] = true
	return nil
}

// Containers returns a snapshot, for assertions.
func (f *Fake) Containers() []Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Container, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Networks returns the network names that exist, for assertions.
func (f *Fake) Networks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.networks))
	for n := range f.networks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Fake must satisfy Client.
var _ Client = (*Fake)(nil)
