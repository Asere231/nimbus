package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Naming containers and finding ports.
//
// Every engine that starts a container needs the same two things: a name
// that is legal, readable in `docker ps`, and stable across restarts; and
// a host port nothing else is using. Neither is specific to Kubernetes or
// to any cloud, so they live beside the client rather than in one engine
// that the others would have to import.

// Labels Nimbus puts on the containers it starts, so they can be found
// again after a restart.
const (
	LabelCluster  = "nimbus.cluster"
	LabelRegistry = "nimbus.registry"
	LabelResource = "nimbus.resource-id"
	LabelRole     = "nimbus.role"
	LabelFunction = "nimbus.function"
	LabelService  = "nimbus.service"
	LabelInstance = "nimbus.instance"
)

// Name derives a stable, legal container name from a resource id.
//
// Resource ids are long and contain slashes -- an ARM path, an ARN -- so
// the name is the resource's own name plus a short hash of the full id:
// short enough to read, and unique enough that two things called the same
// in different resource groups or accounts do not collide.
func Name(prefix, resourceID, name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(resourceID)))
	return fmt.Sprintf("%s-%s-%s", prefix, SanitiseName(name), hex.EncodeToString(sum[:4]))
}

// SanitiseName reduces a name to what Docker will accept.
func SanitiseName(n string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(n) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "nimbus"
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

// FreePort finds a usable host port at or above base, by asking the OS to
// bind it.
//
// Binding is the only reliable check: a registry of "ports we handed out"
// would not notice something else on the machine already using one.
func FreePort(base int) (int, error) {
	for port := base; port < base+500; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port found between %d and %d", base, base+500)
}

// HostPort is the host port a container publishes its containerPort on, or
// 0 if it does not publish it.
//
// Ports maps "8080/tcp" to "addr:port", so the host port is what follows the
// LAST colon -- an IPv6 bind address has its own.
//
// Every engine that adopts a container after a restart needs this: the port
// is the one thing about a running container that cannot be derived, and an
// adopted function or service without it is one nothing can reach.
func HostPort(c Container, containerPort string) int {
	hostPort := c.Ports[containerPort]
	i := strings.LastIndex(hostPort, ":")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(hostPort[i+1:])
	if err != nil {
		return 0
	}
	return n
}
