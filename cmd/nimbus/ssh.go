package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// `nimbus ssh` -- a shell on an instance.
//
// It is not SSH. Nimbus is stdlib-only, so it cannot implement an SSH
// server, and no vendor-official base image ships sshd; an instance is a
// container, and a shell on a container is `docker exec`. The command is
// called `ssh` because that is the thing a reader is looking for, and the
// help text says plainly what it does instead.
//
// It is also cloud-neutral, and stays that way without a per-cloud table:
// it asks the running Nimbus for every resource and picks the ones whose
// body records a backing container. An EC2 instance records
// `nimbusContainer`; when GCE and Azure instances record theirs the same
// way, this works for them with no change here.

func sshCLI(args []string) {
	fs := flag.NewFlagSet("ssh", flag.ExitOnError)
	uiPort := fs.Int("ui-port", 4700, "the port Nimbus serves the visualizer on")
	host := fs.String("host", "localhost", "hostname Nimbus is reachable on")
	shell := fs.String("shell", "", "shell to run (default: sh, or bash if the image has it)")

	// The instance name comes before the flags, because Go's flag package
	// stops at the first non-flag argument: `ssh web --shell bash` would
	// otherwise leave --shell unparsed, which is how anyone would type it.
	// Everything after `--` is the command to run instead of a shell.
	name, rest := "", args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	var command []string
	for i, a := range rest {
		if a == "--" {
			command = rest[i+1:]
			rest = rest[:i]
			break
		}
	}
	_ = fs.Parse(rest)
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: nimbus ssh INSTANCE [--shell sh] [-- command...]")
		fmt.Fprintln(os.Stderr, "\nGives you a shell on an instance. This is `docker exec`, not SSH:")
		fmt.Fprintln(os.Stderr, "Nimbus cannot run an SSH server, so there is no sshd to connect to.")
		os.Exit(2)
	}

	base := fmt.Sprintf("http://%s:%d", *host, *uiPort)
	container, err := findInstanceContainer(base, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}

	// A command gets no TTY; an interactive shell does.
	//
	// There is deliberately no "is stdin a terminal?" check. Detecting
	// that needs an ioctl the standard library does not expose, and the
	// obvious stand-in is wrong: /dev/null IS a character device, so
	// os.ModeCharDevice says "terminal" for a process whose stdin is
	// /dev/null -- which is how this first failed, with docker answering
	// "cannot attach stdin to a TTY-enabled container". Asking for a shell
	// means someone is typing; asking for a command means nobody is.
	dockerArgs := []string{"exec", "-i"}
	if len(command) == 0 {
		dockerArgs = append(dockerArgs, "-t")
		sh := *shell
		if sh == "" {
			sh = "sh"
		}
		command = []string{sh}
	}
	dockerArgs = append(dockerArgs, container)
	dockerArgs = append(dockerArgs, command...)

	cmd := exec.Command("docker", dockerArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "nimbus: running docker exec: %v\n", err)
		os.Exit(1)
	}
}

// findInstanceContainer asks the running Nimbus which container backs an
// instance, by name or by id.
func findInstanceContainer(base, want string) (string, error) {
	resp, err := http.Get(base + "/api/resources")
	if err != nil {
		return "", fmt.Errorf("asking Nimbus for its resources: %w\n\n"+
			"Is it running? Start it with: nimbus start", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Nimbus answered %s for /api/resources", resp.Status)
	}

	var resources []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
		Body struct {
			Container string `json:"nimbusContainer"`
		} `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		return "", fmt.Errorf("reading Nimbus's resources: %w", err)
	}

	var names []string
	for _, r := range resources {
		if r.Body.Container == "" {
			continue // not something with a shell
		}
		names = append(names, r.Name)
		if strings.EqualFold(r.Name, want) || strings.EqualFold(r.ID, want) {
			return r.Body.Container, nil
		}
	}

	if len(names) == 0 {
		return "", fmt.Errorf("no instance %q: nothing running has a shell", want)
	}
	sort.Strings(names)
	return "", fmt.Errorf("no instance %q. Running: %s", want, strings.Join(names, ", "))
}
