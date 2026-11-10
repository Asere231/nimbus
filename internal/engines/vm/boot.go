package vm

import "strings"

// The bootstrap an instance runs.
//
// Two decisions in here are worth more than the script itself.
//
// **User data travels in an environment variable, base64-encoded, and is
// decoded inside the container.** It is not interpolated into this command.
// User data is whatever someone wrote -- quotes, backticks, dollar signs,
// newlines -- and splicing it into an `sh -c` string would let an ordinary
// script break the boot, or run something the bootstrap meant to keep
// separate. The same goes for the authorized-keys file. `base64 -d` is in
// busybox and in coreutils, so this works on alpine and on ubuntu alike.
//
// **Nothing here uses `set -e`, and the script ends in a blocking loop.**
// A machine does not exit when its boot script finishes, and a container
// that exits reads as an instance that crashed seconds after the apply --
// which looks like a Nimbus bug and is the first thing anyone would hit.
// Real EC2 does not terminate an instance whose user data failed either, so
// a failing script is reported and stepped over rather than fatal.
//
// `sleep infinity` is deliberately avoided: busybox does not accept it.
const bootScript = `
mkdir -p /var/lib/nimbus /var/log /root/.ssh 2>/dev/null
if [ -n "$NIMBUS_SSH_KEYS" ]; then
  printf '%s' "$NIMBUS_SSH_KEYS" | base64 -d > /root/.ssh/authorized_keys 2>/dev/null
  chmod 700 /root/.ssh 2>/dev/null
  chmod 600 /root/.ssh/authorized_keys 2>/dev/null
fi
echo "nimbus: instance booting"
if [ -n "$NIMBUS_USER_DATA" ]; then
  printf '%s' "$NIMBUS_USER_DATA" | base64 -d > /var/lib/nimbus/user-data 2>/dev/null
  chmod +x /var/lib/nimbus/user-data 2>/dev/null
  echo "nimbus: running user data"
  sh /var/lib/nimbus/user-data 2>&1 | tee /var/log/nimbus-user-data.log
  echo "nimbus: user data finished"
fi
echo "nimbus: instance ready"
while true; do sleep 3600; done
`

// bootCommand is the container's command: a shell and the script above.
//
// /bin/sh rather than bash, because alpine has no bash, and the script is
// POSIX for the same reason.
func bootCommand() []string {
	return []string{"/bin/sh", "-c", strings.TrimSpace(bootScript)}
}

// authorizedKeys joins public keys into the file's format: one per line.
func authorizedKeys(keys []string) string {
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			clean = append(clean, k)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	return strings.Join(clean, "\n") + "\n"
}
