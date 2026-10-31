// Package docker runs containers for the resource types Nimbus backs with a
// real local engine — k3s for AKS, and later a registry for ACR.
//
// It drives the `docker` CLI with exec.Command rather than the Engine API.
// The CLI is the same on macOS, Windows and Linux, it already knows where the
// daemon socket is (a named pipe on Windows, a socket path that differs
// between Docker Desktop, Colima and OrbStack on macOS), and it keeps the main
// module stdlib-only as CLAUDE.md requires — the Engine API would mean either
// a third-party client or hand-rolling the socket transport per platform.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Client is the slice of Docker that Nimbus needs. It is an interface so tests
// can drive a Fake and run without a daemon.
type Client interface {
	// Available reports whether a usable daemon is reachable.
	Available(ctx context.Context) error
	// Run starts a detached container and returns its ID.
	Run(ctx context.Context, opts RunOptions) (string, error)
	// Stop stops a container, ignoring one that is already stopped.
	Stop(ctx context.Context, name string) error
	// Remove force-removes a container, ignoring one that is already gone.
	Remove(ctx context.Context, name string) error
	// Exec runs a command inside a container and returns its stdout.
	Exec(ctx context.Context, name string, cmd ...string) ([]byte, error)
	// Inspect returns what Nimbus needs to know about one container.
	Inspect(ctx context.Context, name string) (Container, error)
	// List returns containers carrying the given label, running or not.
	List(ctx context.Context, label string) ([]Container, error)
	// EnsureNetwork creates a user-defined network if it does not exist.
	EnsureNetwork(ctx context.Context, name string) error
	// RemoveNetwork deletes a network, ignoring one that is already gone.
	RemoveNetwork(ctx context.Context, name string) error
	// ImagePresent reports whether an image is already pulled.
	ImagePresent(ctx context.Context, image string) (bool, error)
	// Pull fetches an image.
	Pull(ctx context.Context, image string) error
}

// RunOptions describes a container to start.
type RunOptions struct {
	Name       string
	Image      string
	Args       []string          // command and arguments passed to the image
	Privileged bool              // k3s needs this
	Ports      map[string]string // "127.0.0.1:6443" -> "6443"
	Labels     map[string]string
	Env        map[string]string
	Network    string
	Volumes    map[string]string // host path or volume name -> container path
	TmpFS      []string
}

// Container is the subset of `docker inspect` Nimbus uses.
type Container struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Status  string
	Labels  map[string]string
	Ports   map[string]string // container port -> host "addr:port"
}

// ErrUnavailable means no usable Docker daemon. Callers turn this into an ARM
// error so the user sees "start Docker", not a timeout.
var ErrUnavailable = errors.New("docker is not available")

// NotFoundError means the container or network does not exist.
type NotFoundError struct{ Name string }

func (e *NotFoundError) Error() string { return "docker: no such object: " + e.Name }

// CLI is the real Client, backed by the docker binary.
type CLI struct {
	// Binary is the executable to run; defaults to "docker".
	Binary string
	// Timeout bounds any single docker invocation. Without it a daemon that
	// is starting up, or a hung pull, would block an ARM request forever --
	// and a hang is a much worse failure than an error, because Terraform
	// just sits there.
	Timeout time.Duration
}

// NewCLI returns a Client using the docker binary on PATH.
func NewCLI() *CLI { return &CLI{Binary: "docker", Timeout: 2 * time.Minute} }

func (c *CLI) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "docker"
}

func (c *CLI) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Minute
}

// run executes a docker command and returns stdout, mapping the two failures
// callers care about onto typed errors.
func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, c.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}

	msg := strings.TrimSpace(stderr.String())
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, fmt.Errorf("%w: the docker command was not found on PATH", ErrUnavailable)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%w: docker %s timed out after %s", ErrUnavailable, args[0], c.timeout())
	case isDaemonDown(msg):
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, firstLine(msg))
	case isNotFound(msg):
		return nil, &NotFoundError{Name: lastArg(args)}
	}
	return nil, fmt.Errorf("docker %s: %s", strings.Join(args, " "), firstLine(msg))
}

// isDaemonDown spots the daemon being unreachable. The wording differs between
// Docker Desktop, Colima, Podman and Rancher Desktop, so several are matched
// rather than one exact string.
func isDaemonDown(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"error during connect",
		"docker daemon is not running",
		"connection refused",
		"no such host",
		"permission denied while trying to connect",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func isNotFound(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such container") ||
		strings.Contains(s, "no such object") ||
		strings.Contains(s, "no such network")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "unknown docker error"
	}
	return s
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

// Available checks for a reachable daemon.
func (c *CLI) Available(ctx context.Context) error {
	// `docker version --format {{.Server.Version}}` fails when only the client
	// is present, which is exactly the case we want to catch: a CLI installed
	// but Docker Desktop not started.
	short := &CLI{Binary: c.binary(), Timeout: 10 * time.Second}
	out, err := short.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("%w: the daemon reported no version", ErrUnavailable)
	}
	return nil
}

// Run starts a detached container.
func (c *CLI) Run(ctx context.Context, opts RunOptions) (string, error) {
	args := []string{"run", "-d", "--name", opts.Name}
	if opts.Privileged {
		args = append(args, "--privileged")
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	for host, container := range opts.Ports {
		args = append(args, "-p", host+":"+container)
	}
	for k, v := range opts.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	for src, dst := range opts.Volumes {
		args = append(args, "-v", src+":"+dst)
	}
	for _, t := range opts.TmpFS {
		args = append(args, "--tmpfs", t)
	}
	args = append(args, opts.Image)
	args = append(args, opts.Args...)

	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Stop stops a container; one that is already gone is not an error.
func (c *CLI) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, "stop", name)
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// Remove force-removes a container; one that is already gone is not an error.
func (c *CLI) Remove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "rm", "-f", "-v", name)
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// Exec runs a command inside a container.
func (c *CLI) Exec(ctx context.Context, name string, cmd ...string) ([]byte, error) {
	return c.run(ctx, append([]string{"exec", name}, cmd...)...)
}

// inspectJSON is the shape of `docker inspect` that Nimbus reads.
type inspectJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// Inspect returns one container's details.
func (c *CLI) Inspect(ctx context.Context, name string) (Container, error) {
	out, err := c.run(ctx, "inspect", name)
	if err != nil {
		return Container{}, err
	}
	var list []inspectJSON
	if err := json.Unmarshal(out, &list); err != nil {
		return Container{}, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	if len(list) == 0 {
		return Container{}, &NotFoundError{Name: name}
	}
	return toContainer(list[0]), nil
}

func toContainer(j inspectJSON) Container {
	ports := map[string]string{}
	for port, bindings := range j.NetworkSettings.Ports {
		if len(bindings) > 0 {
			ports[port] = bindings[0].HostIP + ":" + bindings[0].HostPort
		}
	}
	return Container{
		ID:      j.ID,
		Name:    strings.TrimPrefix(j.Name, "/"),
		Image:   j.Config.Image,
		Running: j.State.Running,
		Status:  j.State.Status,
		Labels:  j.Config.Labels,
		Ports:   ports,
	}
}

// List returns containers carrying a label, running or not.
func (c *CLI) List(ctx context.Context, label string) ([]Container, error) {
	out, err := c.run(ctx, "ps", "-a", "--filter", "label="+label, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var found []Container
	for _, name := range strings.Fields(string(out)) {
		cont, err := c.Inspect(ctx, name)
		var nf *NotFoundError
		if errors.As(err, &nf) {
			continue // removed between ps and inspect
		}
		if err != nil {
			return nil, err
		}
		found = append(found, cont)
	}
	return found, nil
}

// EnsureNetwork creates a network if it is missing.
func (c *CLI) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "network", "inspect", name); err == nil {
		return nil
	} else if errors.Is(err, ErrUnavailable) {
		return err
	}
	_, err := c.run(ctx, "network", "create", name)
	// A concurrent create is a race we can ignore: the network now exists.
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return err
}

// RemoveNetwork deletes a network; one that is already gone is not an error.
func (c *CLI) RemoveNetwork(ctx context.Context, name string) error {
	_, err := c.run(ctx, "network", "rm", name)
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// ImagePresent reports whether an image has already been pulled, so an offline
// run can tell "not pulled yet" from "pull failed".
func (c *CLI) ImagePresent(ctx context.Context, image string) (bool, error) {
	out, err := c.run(ctx, "image", "inspect", image, "--format", "{{.Id}}")
	if err != nil {
		var nf *NotFoundError
		if errors.As(err, &nf) {
			return false, nil
		}
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// Pull fetches an image.
func (c *CLI) Pull(ctx context.Context, image string) error {
	_, err := c.run(ctx, "pull", image)
	return err
}
