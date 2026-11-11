package vm

import "strings"

// BootLogPath is where an instance's boot output lands, on every cloud.
// Exported because the engine waits for it and `nimbus ssh` examples name
// it: one definition, so the script and the wait cannot drift apart.
const BootLogPath = "/var/log/nimbus-boot.log"

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
// The paths are deliberately NOT named after any cloud. EC2 calls this
// user data, GCE calls it a startup script and Azure calls it custom data;
// the engine calls it the boot script and writes /var/log/nimbus-boot.log,
// so an example on any cloud points at the same file. Naming it
// user-data.log was AWS's vocabulary inside a cloud-neutral engine, which
// the GCE example found by looking for a file that was not there.
//
// One sharp edge, measured rather than imagined: a boot script that
// REDIRECTS INTO /var/log/nimbus-boot.log fights the `tee` that captures
// it. Both open the file, the script's stdout then goes to its own
// redirect rather than through the pipe, and what is left is a
// zero-length file -- while the console (`docker logs`) still has
// everything. The console is the source of truth; the file is a
// convenience. Examples echo to stdout and let the capture do the writing.
//
// The log file is TRUNCATED INTO EXISTENCE before anything else runs, and
// the boot script's output is appended rather than written. Without that,
// the file does not exist until the script's pipeline starts, and the
// instance is reachable before then: CI ran `nimbus ssh NAME -- cat
// /var/log/nimbus-boot.log` forty milliseconds after a successful apply
// and got "No such file or directory". The example offers that very
// command as a Terraform output, so it has to work the moment the apply
// returns. An instance with no boot script gets an empty log rather than
// none, which is the honest answer to "what did it print".
//
// A readiness probe polling for the file was written and then thrown away:
// the Docker Fake refuses an uncanned exec, so every test that forgot to
// allow one would have burned the whole timeout and still PASSED -- silent
// slowness instead of a failure, in every future test. Creating the file
// as the bootstrap's second act fixes the measured problem; anything
// wanting the FINISHED log should wait for `nimbus: boot script finished`
// on the console, which is a definite signal rather than a timing guess.
//
// `sleep infinity` is deliberately avoided: busybox does not accept it.
const bootScript = `
mkdir -p /var/lib/nimbus /var/log /root/.ssh 2>/dev/null
: > ` + BootLogPath + `
if [ -n "$NIMBUS_SSH_KEYS" ]; then
  printf '%s' "$NIMBUS_SSH_KEYS" | base64 -d > /root/.ssh/authorized_keys 2>/dev/null
  chmod 700 /root/.ssh 2>/dev/null
  chmod 600 /root/.ssh/authorized_keys 2>/dev/null
fi
echo "nimbus: instance booting"
if [ -n "$NIMBUS_USER_DATA" ]; then
  printf '%s' "$NIMBUS_USER_DATA" | base64 -d > /var/lib/nimbus/boot-script 2>/dev/null
  chmod +x /var/lib/nimbus/boot-script 2>/dev/null
  echo "nimbus: running the boot script"
  sh /var/lib/nimbus/boot-script 2>&1 | tee -a ` + BootLogPath + `
  echo "nimbus: boot script finished"
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
