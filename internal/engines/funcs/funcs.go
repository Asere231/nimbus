// Package funcs runs serverless handlers for real, in containers.
//
// A function is a directory of code bind-mounted into a runtime image, with
// one long-lived container per function answering invocations over HTTP.
// That is exactly how AWS's own base images work locally: every
// public.ecr.aws/lambda/<runtime> image ships the Runtime Interface
// Emulator, which listens on 8080 and takes an invocation at
//
//	POST /2015-03-31/functions/function/invocations
//
// So Nimbus does not interpret anything. The handler is executed by the
// same runtime interface client that runs it in production, and a Python
// traceback here is the traceback you would get there.
//
// Nothing in this package knows what cloud it is serving. The caller
// supplies the image, the command, the environment and the directory; the
// runtime-to-image mapping is AWS's business and lives in the plugin, so
// Azure Functions could use this engine with a different table.
package funcs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// The path the Runtime Interface Emulator serves. The literal "function" in
// the middle is not a placeholder: RIE always uses that name, whatever the
// function is really called.
const invokePath = "/2015-03-31/functions/function/invocations"

// Config is how a cloud sets the engine up.
type Config struct {
	// NamePrefix starts every container name, so one cloud's containers are
	// recognisable and another cloud's are left alone.
	NamePrefix string
	// PortBase is the first host port tried for a function's RIE.
	PortBase int
	// StartTimeout bounds how long a container may take to answer.
	StartTimeout time.Duration
	// KeepRunning leaves containers up when Nimbus stops.
	KeepRunning bool
	// Dir is where unpacked function code is kept. Required: a bind mount
	// needs a real directory on the host.
	Dir string
}

const (
	DefaultPortBase     = 9100
	DefaultStartTimeout = 2 * time.Minute
)

func (c Config) withDefaults() Config {
	if c.NamePrefix == "" {
		c.NamePrefix = "nimbus-fn"
	}
	if c.PortBase == 0 {
		c.PortBase = DefaultPortBase
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = DefaultStartTimeout
	}
	return c
}

// Function is one running handler.
type Function struct {
	// ResourceID is the caller's identifier for it -- an ARN -- which is
	// what the engine's registry is keyed on.
	ResourceID string
	Name       string
	Image      string
	// Command is what the image is given: for an AWS base image this is the
	// handler, "app.handler".
	Command []string
	Env     map[string]string
	// Dir is the unpacked code, mounted at /var/task.
	Dir       string
	Container string
	Port      int
	// Started is when the container came up, so a log read can ask for
	// everything since then.
	Started time.Time
}

// Engine starts, invokes and stops functions for one cloud.
type Engine struct {
	cfg    Config
	docker docker.Client

	mu    sync.Mutex
	funcs map[string]*Function
}

func New(cfg Config, d docker.Client) *Engine {
	return &Engine{cfg: cfg.withDefaults(), docker: d, funcs: map[string]*Function{}}
}

// SetDocker swaps the Docker client, for tests.
func (e *Engine) SetDocker(d docker.Client) { e.docker = d }

// Docker exposes the client for the few things a plugin does itself.
func (e *Engine) Docker() docker.Client { return e.docker }

// Get returns a function the engine is running, or nil.
func (e *Engine) Get(resourceID string) *Function {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.funcs[resourceID]
}

// Put registers a function the engine should treat as running. Start does
// this itself; a plugin's tests use it to stand a container in.
func (e *Engine) Put(fn *Function) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.funcs[fn.ResourceID] = fn
}

// All returns every function, in resource-id order so a shutdown is
// deterministic.
func (e *Engine) All() []*Function {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Function, 0, len(e.funcs))
	for _, fn := range e.funcs {
		out = append(out, fn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out
}

// CodeDir is where a function's unpacked code goes. One directory per
// resource id, hashed, because a function name is not a safe path and an
// ARN certainly is not.
func (e *Engine) CodeDir(resourceID string) string {
	return filepath.Join(e.cfg.Dir, docker.SanitiseName(hashOf(resourceID)))
}

// Start unpacks nothing and assumes the directory is already filled: the
// caller writes the code, because only it knows where the zip came from.
// Start runs the container and waits until it answers.
func (e *Engine) Start(ctx context.Context, fn *Function) error {
	if err := e.docker.Available(ctx); err != nil {
		return fmt.Errorf("%w\n\nNimbus runs functions as containers, so Docker must be "+
			"running. Start Docker Desktop (or your runtime) and try again.", err)
	}
	if present, err := e.docker.ImagePresent(ctx, fn.Image); err == nil && !present {
		if err := e.docker.Pull(ctx, fn.Image); err != nil {
			return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
				fn.Image, err, fn.Image)
		}
	}

	port, err := docker.FreePort(e.cfg.PortBase)
	if err != nil {
		return err
	}
	fn.Port = port
	fn.Container = docker.Name(e.cfg.NamePrefix, fn.ResourceID, fn.Name)

	// A container left over from a previous run is removed rather than
	// reused: its bind mount points at the old code, and the whole point of
	// an update is that the next invocation runs the new code.
	_ = e.docker.Remove(ctx, fn.Container)

	if _, err := e.docker.Run(ctx, docker.RunOptions{
		Name:    fn.Container,
		Image:   fn.Image,
		Args:    fn.Command,
		Ports:   map[string]string{fmt.Sprintf("127.0.0.1:%d", port): "8080"},
		Env:     fn.Env,
		Volumes: map[string]string{fn.Dir: "/var/task"},
		Labels: map[string]string{
			docker.LabelFunction: fn.Name,
			docker.LabelResource: fn.ResourceID,
			docker.LabelRole:     "function",
		},
	}); err != nil {
		return fmt.Errorf("starting the function container: %w", err)
	}
	fn.Started = time.Now()

	if err := e.waitReady(ctx, fn); err != nil {
		return err
	}
	e.mu.Lock()
	e.funcs[fn.ResourceID] = fn
	e.mu.Unlock()
	return nil
}

// waitReady blocks until the emulator answers.
//
// Readiness is measured by a real invocation of an empty payload, because
// RIE opens its port before the runtime client has loaded the handler: a
// connection succeeding proves nothing, and the first real invocation would
// then fail with a connection reset that looks like the handler's fault.
func (e *Engine) waitReady(ctx context.Context, fn *Function) error {
	deadline := time.Now().Add(e.cfg.StartTimeout)
	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", fn.Port, invokePath)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("the function container did not answer within %s: %v\n\n"+
		"Check `docker logs %s`", e.cfg.StartTimeout, last, fn.Container)
}

// Result is one invocation's outcome.
type Result struct {
	// Payload is what the handler returned, or the error document it
	// produced, byte for byte.
	Payload []byte
	// FunctionError is "Unhandled" when the handler raised, empty
	// otherwise. It is a HEADER on the response, not part of the payload,
	// which is how a client tells a thrown exception from a returned value
	// that happens to describe one.
	FunctionError string
	Logs          string
	Duration      time.Duration
}

// Invoke runs the handler once.
func (e *Engine) Invoke(ctx context.Context, fn *Function, payload []byte, timeout time.Duration) (*Result, error) {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", fn.Port, invokePath)
	// The HTTP timeout is the function's own timeout plus a moment, so a
	// handler that runs to its limit is reported as a function timeout
	// rather than as a transport error.
	client := &http.Client{Timeout: timeout + 5*time.Second}

	since := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invoking %s: %w", fn.Name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	out := &Result{
		Payload:       body,
		FunctionError: resp.Header.Get("X-Amz-Function-Error"),
		Duration:      time.Since(start),
	}
	// Whatever the handler printed went to the container's stdout, which is
	// where the log group's lines come from.
	if logs, err := e.docker.Logs(ctx, fn.Container, since); err == nil {
		out.Logs = string(logs)
	}
	return out, nil
}

// Stop removes a function's container, unless it is meant to outlive us.
func (e *Engine) Stop(ctx context.Context, fn *Function) {
	e.mu.Lock()
	delete(e.funcs, fn.ResourceID)
	e.mu.Unlock()
	if e.cfg.KeepRunning {
		return
	}
	_ = e.docker.Remove(ctx, fn.Container)
}

// Forget drops a function from the registry without touching its
// container, for a reattach that found it already gone.
func (e *Engine) Forget(resourceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.funcs, resourceID)
}

// RemoveCode deletes a function's unpacked code.
func (e *Engine) RemoveCode(resourceID string) {
	dir := e.CodeDir(resourceID)
	if dir != "" && dir != e.cfg.Dir {
		_ = os.RemoveAll(dir)
	}
}
