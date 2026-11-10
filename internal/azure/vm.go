package azure

import (
	"context"
	"encoding/base64"
	"log"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/engines/vm"
)

// Virtual machines, which really run: one container per VM, on the
// cloud-neutral engine in internal/engines/vm.
//
// Azure started from the opposite place to the other two clouds. The type
// is already in the spec table (internal/azure/spectypes_gen.go), so an
// `azurerm_linux_virtual_machine` applied before a line of this file
// existed -- in one second, with a NIC and an OS disk, because ARM's
// generic path already stored the JSON. What was missing was that nothing
// ran. This file is about making it real, not about making it accepted.
//
// Four things measured against azurerm 4.x, all of which differ from EC2
// and GCE:
//
// **azurerm validates the SSH key client-side.** It decodes
// admin_ssh_key.public_key and fails with `decoding
// "admin_ssh_key.0.public_key" for public key data` before any request is
// sent. The aws provider accepted an obviously fake key; this one does
// not, so the example carries a real (throwaway) public key.
//
// **An empty object echoed back is not the same as absent.** azurerm sends
// `additionalCapabilities: {}` and, reading that back, flattens it into an
// `additional_capabilities` block with hibernation_enabled = false and
// ultra_ssd_enabled = false -- which its configuration does not have, so
// every plan wants to remove it. Measured: a clean apply followed by a
// dirty plan, with nothing in the diff explaining where the block came
// from. So an empty one is dropped.
//
// **The image is a publisher/offer/sku triple**, not an id and not a
// single name: Canonical / ubuntu-24_04-lts / server. Like GCE's image
// name and unlike an AMI id it says what it is, so the container image is
// derived from it rather than needing a tag.
//
// **Custom data is base64 in the body**, the same shape EC2's UserData
// arrives in, where GCE puts a startup script in metadata.

const typeVirtualMachine = "Microsoft.Compute/virtualMachines"

// DefaultVMImage backs an image reference nothing else matches.
const DefaultVMImage = "alpine:3.20"

// vmImageAliases maps the offer or sku half of an Azure image reference to
// a container image. Longest match wins.
//
// These are real base images, so a custom-data script that runs apt is
// running the real thing. They are not Azure's images: there is no kernel,
// no waagent and no cloud-init, which is why the list is short rather than
// an attempt at a catalogue.
var vmImageAliases = []struct{ prefix, image string }{
	{"ubuntu-24_04", "ubuntu:24.04"},
	{"ubuntu-22_04", "ubuntu:22.04"},
	{"ubuntu-20_04", "ubuntu:20.04"},
	{"0001-com-ubuntu-server-jammy", "ubuntu:22.04"},
	{"0001-com-ubuntu-server-noble", "ubuntu:24.04"},
	{"ubuntuserver", "ubuntu:22.04"},
	{"ubuntu", "ubuntu:24.04"},
	{"debian-12", "debian:12"},
	{"debian-11", "debian:11"},
	{"debian", "debian:12"},
	{"rhel", "rockylinux:9"},
	{"centos", "rockylinux:9"},
	{"almalinux", "almalinux:9"},
	{"alpine", "alpine:3.20"},
}

// vmDockerImage reads the publisher/offer/sku triple and picks a container
// image. The offer is tried first, then the sku, then the publisher.
func vmDockerImage(body map[string]any) string {
	ref := objAt(body, "properties", "storageProfile", "imageReference")
	for _, key := range []string{"offer", "sku", "publisher"} {
		name := strings.ToLower(strings.TrimSpace(str(ref[key])))
		if name == "" {
			continue
		}
		for _, a := range vmImageAliases {
			if strings.HasPrefix(name, a.prefix) {
				return a.image
			}
		}
	}
	// A custom image, a gallery image or a disk: nothing to read a name
	// out of, so the default stands.
	return DefaultVMImage
}

func registerVMHooks(s *Server) {
	s.registerHook(typeVirtualMachine, Hook{
		BeforePut: func(s *Server, t target, body map[string]any, existing map[string]any) *armError {
			s.fillVM(t, body)
			return nil
		},
		// Provision, not AfterPut: the write answers at once with
		// provisioningState "Creating" and an operation to poll, and the
		// resource reaches Succeeded when the container is really up.
		//
		// Measured why it matters: with AfterPut the apply finished in
		// about a second and the container was still being pulled, so
		// `nimbus ssh` right after a successful apply found nothing
		// running. EC2 waits for the instance to report `running` and GCE
		// polls its operation; azurerm waits on this. Real Azure takes
		// minutes over a VM, so waiting is also the honest shape.
		Provision: func(s *Server, t target, body map[string]any) error {
			return s.bootVM(t.ID, body)
		},
		Deprovision: func(s *Server, t target) {
			s.deleteVMContainer(t.ID)
		},
		// AfterDelete as well as Deprovision, and deliberately: this one
		// runs SYNCHRONOUSLY, before the delete is answered, so a
		// `terraform destroy` cannot return with the container still up.
		// Measured on GCE, where doing it in the background did exactly
		// that -- the GKE node pool's lesson. Removing a container is
		// fast; only a create is slow, because it pulls an image.
		// Deprovision then has nothing left to do, which is why it is
		// idempotent.
		AfterDelete: func(s *Server, t target, body map[string]any) {
			s.deleteVMContainer(t.ID)
		},
		Backing: "real",
	})
}

// fillVM sets what the server owns, and prunes what azurerm must not read
// back.
func (s *Server) fillVM(t target, body map[string]any) {
	props, ok := body["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		body["properties"] = props
	}

	// Derived, never generated: azurerm re-reads vmId on every refresh, so
	// a fresh GUID each time would be a permanent diff. The same rule as
	// the AKS kubeconfig and an etag.
	props["vmId"] = identityGUID(t.ID, "vmId")
	props["timeCreated"] = time.Now().UTC().Format(time.RFC3339)
	props["provisioningState"] = "Succeeded"

	// An empty additionalCapabilities is DROPPED. azurerm sends `{}` and
	// then flattens what it reads back into a block with two false fields
	// that its configuration does not have -- a clean apply followed by a
	// plan that wants to remove something nobody wrote. Absent is the only
	// answer that round-trips.
	if caps, ok := props["additionalCapabilities"].(map[string]any); ok && len(caps) == 0 {
		delete(props, "additionalCapabilities")
	}

	// The OS disk gets the name Azure would give it, so a client reading
	// osDisk.name finds one.
	if storageProfile, ok := props["storageProfile"].(map[string]any); ok {
		if osDisk, ok := storageProfile["osDisk"].(map[string]any); ok {
			if str(osDisk["name"]) == "" {
				osDisk["name"] = t.Name + "_OsDisk_1_" + strings.ReplaceAll(identityGUID(t.ID, "osDisk"), "-", "")
			}
			if str(osDisk["createOption"]) == "" {
				osDisk["createOption"] = "FromImage"
			}
			if str(osDisk["osType"]) == "" {
				osDisk["osType"] = "Linux"
			}
			if _, has := osDisk["diskSizeGB"]; !has {
				osDisk["diskSizeGB"] = float64(30)
			}
		}
	}
}

// bootVM starts the container and records it.
func (s *Server) bootVM(id string, body map[string]any) error {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if !s.store.Exists(id) {
		return nil // deleted before it finished booting
	}

	ctx, cancel := context.WithTimeout(context.Background(), vmStartTimeout)
	defer cancel()

	props, _ := body["properties"].(map[string]any)
	osProfile := objAt(body, "properties", "osProfile")

	// Custom data is base64 in the body, like EC2's UserData.
	script := ""
	if raw := str(osProfile["customData"]); raw != "" {
		if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
			script = string(decoded)
		} else {
			script = raw
		}
	}

	m := &vm.Instance{
		ResourceID: id,
		Name:       lastPathSegment(id),
		Image:      vmDockerImage(body),
		UserData:   script,
		PublicKeys: vmPublicKeys(osProfile),
		// Azure's IMDS has no endpoint override the way AWS's and
		// Google's do -- there is no AZURE_METADATA_HOST that any SDK
		// reads -- so a VM is given its metadata as environment
		// variables instead, and that limitation is stated rather than
		// half-emulated. See docs/ROADMAP.md.
		Env: map[string]string{
			"NIMBUS_VM_NAME":     lastPathSegment(id),
			"NIMBUS_VM_ID":       str(props["vmId"]),
			"NIMBUS_VM_LOCATION": str(body["location"]),
			"NIMBUS_VM_SIZE":     str(objAt(props, "hardwareProfile")["vmSize"]),
		},
	}
	if err := s.vms.Start(ctx, m); err != nil {
		log.Printf("nimbus: virtual machine %s failed to start: %v", m.Name, err)
		// Returned, not swallowed: the resource then reaches Failed and
		// the operation carries the reason, which is what a client shows.
		return err
	}
	// Recorded so `nimbus ssh` -- a separate process, which cannot see the
	// engine -- can find the container. The same field name AWS and GCP
	// use, so that command needs no per-cloud table.
	return s.store.Update(id, func(stored map[string]any) error {
		stored["nimbusContainer"] = m.Container
		return nil
	})
}

func (s *Server) deleteVMContainer(id string) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), vmStartTimeout)
	defer cancel()
	if m := s.vms.Get(id); m != nil {
		if err := s.vms.Delete(ctx, m); err != nil {
			log.Printf("nimbus: deleting the virtual machine's container: %v", err)
		}
	}
}

// vmPublicKeys reads the keys out of linuxConfiguration.ssh.publicKeys,
// which is where azurerm puts admin_ssh_key.
func vmPublicKeys(osProfile map[string]any) []string {
	ssh := objAt(osProfile, "linuxConfiguration", "ssh")
	entries, _ := ssh["publicKeys"].([]any)
	var keys []string
	for _, e := range entries {
		if entry, ok := e.(map[string]any); ok {
			if k := str(entry["keyData"]); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// vmStartTimeout bounds starting a container, which includes pulling its
// image -- most of a first run.
const vmStartTimeout = 10 * time.Minute

// objAt walks a path of nested objects, returning an empty map rather than
// nil so a caller can read a field without checking at every level.
func objAt(body map[string]any, path ...string) map[string]any {
	cur := body
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		cur = next
	}
	return cur
}

// lastPathSegment is the resource's own name out of an ARM id.
func lastPathSegment(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}
