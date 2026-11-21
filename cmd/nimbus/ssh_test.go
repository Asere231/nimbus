package main

import (
	"os"
	"strings"
	"testing"
)

// Docker Desktop's parting hint names the CONTAINER, and this command
// exists so that nobody has to think about one.
//
// After an interactive exec it prints "What's next: Try Docker Debug ...
// -> docker debug nimbus-ec2-web-b640338a", which is addressed to
// someone debugging a container and ends a session about an INSTANCE by
// leaking the generated name. Seen on Docker Desktop 29.9.2 on macOS
// while testing `nimbus ssh` by hand -- the interactive form is the one
// part of this command a tool cannot drive, because it allocates a TTY,
// which is why it took a person to notice.
//
// Asserted against the source rather than by running docker: the thing
// worth pinning is that the variable is still SET, and a test that
// needed a daemon would skip itself on every machine without one and
// say nothing -- this repository's "a skipped test passes" trap.
func TestDockerCLIHintsAreSuppressed(t *testing.T) {
	data, err := os.ReadFile("ssh.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	if !strings.Contains(src, "DOCKER_CLI_HINTS=false") {
		t.Error("nimbus ssh does not suppress Docker's CLI hints")
	}
	// Appended to the real environment rather than replacing it: docker
	// needs DOCKER_HOST, PATH and the rest to work at all.
	if !strings.Contains(src, "append(os.Environ()") {
		t.Error("the environment is replaced rather than extended, which would break docker")
	}
}
