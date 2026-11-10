package azure

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A real throwaway public key. azurerm DECODES admin_ssh_key client-side
// and refuses an invalid one -- `decoding "admin_ssh_key.0.public_key" for
// public key data`, before any request is sent -- where the aws provider
// accepted an obviously fake key. Nothing here verifies it; it is real so
// the example that carries it can apply.
const testVMKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFOZQv8spJwsKhE202zjRHC/h/0m10n5m9B4Yg2jaGS7 nimbus@local"

// vmBody is the shape azurerm 4.x PUTs for an azurerm_linux_virtual_machine,
// including the empty additionalCapabilities that caused a dirty plan.
func vmBody(customData string) string {
	return `{
	  "location": "eastus",
	  "properties": {
	    "additionalCapabilities": {},
	    "hardwareProfile": {"vmSize": "Standard_B1s"},
	    "storageProfile": {
	      "imageReference": {
	        "publisher": "Canonical", "offer": "ubuntu-24_04-lts",
	        "sku": "server", "version": "latest"
	      },
	      "osDisk": {"caching": "ReadWrite", "managedDisk": {"storageAccountType": "Standard_LRS"}}
	    },
	    "osProfile": {
	      "adminUsername": "nimbus",
	      "computerName": "vm-nimbus",
	      "customData": "` + base64.StdEncoding.EncodeToString([]byte(customData)) + `",
	      "linuxConfiguration": {
	        "disablePasswordAuthentication": true,
	        "ssh": {"publicKeys": [{"keyData": "` + testVMKey + `", "path": "/home/nimbus/.ssh/authorized_keys"}]}
	      }
	    },
	    "networkProfile": {"networkInterfaces": [{"id": "` + sub + `/resourceGroups/rg-vm/providers/Microsoft.Network/networkInterfaces/nic"}]}
	  }
	}`
}

func vmTestServer(t *testing.T) (*httptest.Server, *Server, *docker.Fake) {
	t.Helper()
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)
	f := docker.NewFake()
	f.Images["ubuntu:24.04"] = true
	f.Images[DefaultVMImage] = true
	f.ExecAny = true
	az.useDocker(f)
	return srv, az, f
}

func putVM(t *testing.T, srv *httptest.Server, az *Server, customData string) (string, map[string]any) {
	t.Helper()
	rg := sub + "/resourceGroups/rg-vm"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.Compute/virtualMachines/vm-nimbus"

	var body map[string]any
	if err := json.Unmarshal([]byte(vmBody(customData)), &body); err != nil {
		t.Fatal(err)
	}
	code, got := do(t, srv, "PUT", id, body)
	if code != 200 && code != 201 {
		t.Fatalf("PUT vm = %d: %v", code, got)
	}
	waitForProvisioning(t, srv, id, "Succeeded")
	_, read := do(t, srv, "GET", id, nil)
	return id, read
}

// An empty additionalCapabilities must NOT come back.
//
// azurerm sends `{}` and then flattens what it reads back into an
// `additional_capabilities` block with hibernation_enabled = false and
// ultra_ssd_enabled = false -- which its configuration does not have, so
// every plan wants to remove it. Measured: a clean apply followed by a
// dirty plan, with nothing in the diff saying where the block came from.
// Absent is the only answer that round-trips.
func TestAnEmptyAdditionalCapabilitiesIsNotEchoedBack(t *testing.T) {
	srv, az, _ := vmTestServer(t)
	_, read := putVM(t, srv, az, "#!/bin/sh\necho hi\n")

	props := read["properties"].(map[string]any)
	if _, present := props["additionalCapabilities"]; present {
		t.Errorf("additionalCapabilities came back as %v; an empty one must be dropped",
			props["additionalCapabilities"])
	}
}

// The image is a publisher/offer/sku TRIPLE, not an id. Like a GCE image
// name and unlike an AMI, it says what it is -- so the container image is
// derived from it and no tag is needed.
func TestTheContainerImageComesFromThePublisherOfferSkuTriple(t *testing.T) {
	srv, az, _ := vmTestServer(t)
	id, _ := putVM(t, srv, az, "#!/bin/sh\necho hi\n")

	m := az.vms.Get(id)
	if m == nil {
		t.Fatal("the engine does not know about the machine")
	}
	if m.Image != "ubuntu:24.04" {
		t.Errorf("container image = %q, want ubuntu:24.04 from Canonical/ubuntu-24_04-lts", m.Image)
	}

	for _, c := range []struct{ publisher, offer, sku, want string }{
		{"Canonical", "ubuntu-24_04-lts", "server", "ubuntu:24.04"},
		{"Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts-gen2", "ubuntu:22.04"},
		{"Debian", "debian-12", "12-gen2", "debian:12"},
		{"RedHat", "RHEL", "9-lvm-gen2", "rockylinux:9"},
		{"SomeVendor", "mystery-appliance", "v1", DefaultVMImage},
	} {
		body := map[string]any{"properties": map[string]any{
			"storageProfile": map[string]any{"imageReference": map[string]any{
				"publisher": c.publisher, "offer": c.offer, "sku": c.sku,
			}},
		}}
		if got := vmDockerImage(body); got != c.want {
			t.Errorf("%s/%s/%s = %q, want %q", c.publisher, c.offer, c.sku, got, c.want)
		}
	}
}

// Custom data is base64 in the body -- the same shape EC2's UserData
// arrives in, where GCE puts a startup script in metadata -- and reaches
// the engine decoded, because that is what a boot script is.
func TestCustomDataIsBase64AndReachesTheEngineDecoded(t *testing.T) {
	srv, az, _ := vmTestServer(t)
	script := "#!/bin/sh\necho \"it's $HOME\"\n"
	id, _ := putVM(t, srv, az, script)

	m := az.vms.Get(id)
	if m == nil {
		t.Fatal("the engine does not know about the machine")
	}
	if m.UserData != script {
		t.Errorf("the engine got %q, want %q", m.UserData, script)
	}
	if len(m.PublicKeys) != 1 || !strings.Contains(m.PublicKeys[0], "nimbus@local") {
		t.Errorf("admin_ssh_key did not reach the machine: %v", m.PublicKeys)
	}
}

// vmId is DERIVED from the resource ID, because azurerm re-reads it on
// every refresh: a fresh GUID each time would be a permanent diff. The
// same rule as the AKS kubeconfig and an etag.
func TestTheVMIdIsDerivedSoItNeverChanges(t *testing.T) {
	srv, az, _ := vmTestServer(t)
	id, read := putVM(t, srv, az, "#!/bin/sh\ntrue\n")

	first := read["properties"].(map[string]any)["vmId"]
	if first == nil || first == "" {
		t.Fatal("no vmId; azurerm reads it and would show a diff")
	}
	for i := 0; i < 3; i++ {
		_, again := do(t, srv, "GET", id, nil)
		if got := again["properties"].(map[string]any)["vmId"]; got != first {
			t.Fatalf("vmId changed between reads: %v then %v", first, got)
		}
	}
}

// The machine provisions ASYNCHRONOUSLY: the write answers at once with
// "Creating" and reaches Succeeded when the container is really up.
//
// Measured why: with the work in AfterPut the apply finished in about a
// second while the image was still being pulled, so `nimbus ssh` right
// after a successful apply found nothing running. Real Azure takes minutes
// over a VM, so waiting is also the honest shape.
func TestAVirtualMachineProvisionsAsynchronously(t *testing.T) {
	srv, az, _ := vmTestServer(t)
	rg := sub + "/resourceGroups/rg-vm-async"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.Compute/virtualMachines/vm-async"

	var body map[string]any
	if err := json.Unmarshal([]byte(vmBody("#!/bin/sh\ntrue\n")), &body); err != nil {
		t.Fatal(err)
	}
	_, created := do(t, srv, "PUT", id, body)
	if state := created["properties"].(map[string]any)["provisioningState"]; state != "Creating" {
		t.Errorf("the create answered provisioningState %v, want Creating", state)
	}
	waitForProvisioning(t, srv, id, "Succeeded")

	// And the container is recorded, which is what `nimbus ssh` reads.
	_, read := do(t, srv, "GET", id, nil)
	if read["nimbusContainer"] == nil {
		t.Error("no nimbusContainer recorded; nimbus ssh could not find it")
	}
	if az.vms.Get(id) == nil {
		t.Error("the engine does not know about the machine")
	}
}

// Deleting removes the container BEFORE the delete is answered, so a
// destroy cannot report success with something still running -- the GKE
// node pool's lesson, which GCE paid for again in this milestone.
func TestDeletingAVirtualMachineStopsItsContainerFirst(t *testing.T) {
	srv, az, f := vmTestServer(t)
	id, _ := putVM(t, srv, az, "#!/bin/sh\ntrue\n")
	container := az.vms.Get(id).Container
	f.Calls = nil

	if code, body := do(t, srv, "DELETE", id, nil); code != 200 && code != 202 && code != 204 {
		t.Fatalf("DELETE = %d: %v", code, body)
	}
	// Already gone by the time the delete answered.
	if !strings.Contains(strings.Join(f.Calls, "; "), "remove "+container) {
		t.Errorf("the container was not removed during the delete: %v", f.Calls)
	}
	if az.vms.Get(id) != nil {
		t.Error("the engine still knows about the machine")
	}
}

func waitForProvisioning(t *testing.T, srv *httptest.Server, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		_, read := do(t, srv, "GET", id, nil)
		if props, ok := read["properties"].(map[string]any); ok {
			last, _ = props["provisioningState"].(string)
			if last == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("provisioningState is %q, want %q", last, want)
}
