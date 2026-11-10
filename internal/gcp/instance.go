package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/vm"
)

// Compute instances, which really run: one container per instance, on the
// cloud-neutral engine in internal/engines/vm.
//
// The exact body google provider 6.50.0 sends, captured from a debug log:
//
//	POST /compute/v1/projects/nimbus-local/zones/us-central1-a/instances
//	{
//	 "canIpForward": false, "deletionProtection": false,
//	 "disks": [{"autoDelete": true, "boot": true, "mode": "READ_WRITE",
//	            "initializeParams": {"sourceImage":
//	              "projects/debian-cloud/global/images/debian-12"}}],
//	 "labels": {"goog-terraform-provisioned": "true"},
//	 "machineType": "projects/nimbus-local/zones/us-central1-a/machineTypes/e2-micro",
//	 "metadata": {"items": [{"key": "ssh-keys", "value": "..."},
//	                        {"key": "startup-script", "value": "..."}]},
//	 "name": "measure-web",
//	 "networkInterfaces": [{"subnetwork":
//	   "projects/nimbus-local/regions/us-central1/subnetworks/measure-subnet"}],
//	 "params": {}, "scheduling": {"automaticRestart": true},
//	 "tags": {"items": ["web", "ssh"]}
//	}
//
// Four things worth knowing about it.
//
// The body is NOT wrapped. GKE's is -- `{"cluster": {...}}` -- and nothing
// else in GCP is, so an instance reads like every other Compute resource.
//
// `metadata` is an ARRAY of key/value pairs under `items`, not an object,
// and that is where `startup-script` and `ssh-keys` live. A startup script
// is metadata in GCE, where it is a separate UserData parameter in EC2.
//
// `tags` is also `{items: [...]}`, and they are network tags -- strings a
// firewall rule matches on -- not the key/value labels EC2 calls tags.
// Labels are the key/value ones, in a separate field.
//
// The lifecycle verbs are TRAILING PATH SEGMENTS: `POST
// .../instances/{name}/stop`. Not colon verbs like GKE's `:setSize`, and
// not query parameters. Three conventions for "do a thing to a resource" in
// one cloud, and this is the third.

// TypeInstance is what the store and the visualizer call one.
const TypeInstance = "compute.googleapis.com/Instance"

// instanceImageKey is the metadata key that overrides which container image
// backs an instance. Rarely needed: unlike an AMI id, a GCE image name says
// what it is, so images.go can usually derive it.
const instanceImageKey = "nimbus-image"

// serveInstances routes everything under a zone's instances collection.
//
// rest is what follows the zone: instances / [name] / [verb].
func (s *Server) serveInstances(w http.ResponseWriter, r *http.Request, scope string, rest []string) string {
	name, verb := "", ""
	if len(rest) > 1 {
		name = rest[1]
	}
	if len(rest) > 2 {
		verb = rest[2]
	}

	switch {
	case name == "" && r.Method == http.MethodPost:
		return s.insertInstance(w, r, scope)
	case name == "" && r.Method == http.MethodGet:
		return s.listInstances(w, scope)
	case verb != "" && r.Method == http.MethodPost:
		return s.instanceVerb(w, scope, name, verb)
	case r.Method == http.MethodGet:
		return s.getInstance(w, scope, name)
	case r.Method == http.MethodDelete:
		return s.deleteInstance(w, scope, name)
	}
	writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
	return "compute instances " + r.Method
}

func (s *Server) instanceLink(scope, name string) string {
	return s.computeLink(scope, "instances", name)
}

func (s *Server) insertInstance(w http.ResponseWriter, r *http.Request, scope string) string {
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		writeError(w, badRequest("reading the instance: %v", err))
		return "bad instance"
	}
	if body == nil {
		body = map[string]any{}
	}
	name := str(body["name"])
	if name == "" {
		writeError(w, badRequest("an instance needs a name"))
		return "instance without a name"
	}
	link := s.instanceLink(scope, name)
	if s.store.Exists(link) {
		writeError(w, alreadyExists("instances/"+name, s.cfg.ProjectID))
		return "instance exists"
	}

	zone := strings.TrimPrefix(scope, "zones/")
	s.fillInstance(body, scope, zone, name, link)

	raw, err := json.Marshal(body)
	if err != nil {
		writeError(w, internalError(err))
		return "encoding the instance"
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: link, Type: TypeInstance, Name: name,
		ParentID: s.instanceParent(body), Location: zone,
		Backing: "real", Body: raw,
	})

	// The operation stays RUNNING until the container answers, and the
	// provider polls it before reading the instance. Answering DONE at once
	// -- which every other Compute write here does -- would hand back a
	// RUNNING instance with nothing running in it.
	op := s.newPendingOperation(scope, "insert", link)
	go s.bootInstance(link, op.SelfLink, body)

	writeJSON(w, http.StatusOK, op)
	return "instance " + name
}

// fillInstance sets what the server owns. Everything a client sent is left
// exactly as it arrived.
func (s *Server) fillInstance(body map[string]any, scope, zone, name, link string) {
	body["kind"] = "compute#instance"
	body["id"] = numericID(link)
	body["name"] = name
	body["selfLink"] = link
	body["creationTimestamp"] = timestamp()
	body["zone"] = s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/" + scope
	body["status"] = "PROVISIONING"
	body["cpuPlatform"] = "Intel Broadwell"
	body["lastStartTimestamp"] = timestamp()

	// A partial machineType URL must come back as a full self link, like a
	// subnetwork's region and network do -- the provider compares it with
	// what it sent and plans a change when it differs.
	if mt := str(body["machineType"]); mt != "" {
		body["machineType"] = s.linkFor(mt)
	} else {
		body["machineType"] = s.computeLink(scope, "machineTypes", "e2-micro")
	}

	// Disks: fill in what the server decides, keeping the client's own
	// fields. The device name and source are what a client reads back.
	disks, _ := body["disks"].([]any)
	for i, d := range disks {
		disk, ok := d.(map[string]any)
		if !ok {
			continue
		}
		device := str(disk["deviceName"])
		if device == "" {
			device = name
			if i > 0 {
				device = fmt.Sprintf("%s-%d", name, i)
			}
			disk["deviceName"] = device
		}
		disk["kind"] = "compute#attachedDisk"
		disk["index"] = float64(i)
		disk["type"] = orDefault(disk["type"], "PERSISTENT")
		disk["interface"] = orDefault(disk["interface"], "SCSI")
		if disk["source"] == nil {
			disk["source"] = s.computeLink(scope, "disks", device)
		}
		if params, ok := disk["initializeParams"].(map[string]any); ok {
			if img := str(params["sourceImage"]); img != "" {
				params["sourceImage"] = s.linkFor(img)
			}
			params["diskSizeGb"] = orDefault(params["diskSizeGb"], "10")
			params["diskType"] = s.computeLink(scope, "diskTypes", "pd-balanced")
		}
	}

	// Network interfaces: the subnetwork arrives as a partial URL and the
	// addresses are the server's to assign.
	nics, _ := body["networkInterfaces"].([]any)
	for i, n := range nics {
		nic, ok := n.(map[string]any)
		if !ok {
			continue
		}
		nic["kind"] = "compute#networkInterface"
		nic["name"] = fmt.Sprintf("nic%d", i)
		if sub := str(nic["subnetwork"]); sub != "" {
			nic["subnetwork"] = s.linkFor(sub)
			// The network follows from the subnetwork, and the provider
			// reads it back even when it only sent the subnetwork.
			if nic["network"] == nil {
				if net := s.networkOfSubnetwork(nic["subnetwork"].(string)); net != "" {
					nic["network"] = net
				}
			}
		} else if net := str(nic["network"]); net != "" {
			nic["network"] = s.linkFor(net)
		}
		// Derived from the instance's own link, so it is the same on every
		// read: a value that differed between reads is a permanent diff.
		nic["networkIP"] = internalIPFor(link)
		nic["stackType"] = orDefault(nic["stackType"], "IPV4_ONLY")
	}

	body["scheduling"] = orDefault(body["scheduling"], map[string]any{})
	if sch, ok := body["scheduling"].(map[string]any); ok {
		sch["onHostMaintenance"] = orDefault(sch["onHostMaintenance"], "MIGRATE")
		sch["automaticRestart"] = orDefault(sch["automaticRestart"], true)
		sch["preemptible"] = orDefault(sch["preemptible"], false)
		sch["provisioningModel"] = orDefault(sch["provisioningModel"], "STANDARD")
	}

	// Metadata keeps whatever items the client sent and gains a
	// fingerprint, which the provider sends back on an update.
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{"items": []any{}}
		body["metadata"] = meta
	}
	meta["kind"] = "compute#metadata"
	meta["fingerprint"] = fingerprintOf(link + "/metadata")
	if tags, ok := body["tags"].(map[string]any); ok {
		tags["fingerprint"] = fingerprintOf(link + "/tags")
	}
	body["labelFingerprint"] = fingerprintOf(link + "/labels")
	body["fingerprint"] = fingerprintOf(link)
}

// instanceParent nests an instance under its subnetwork, so the graph shows
// it inside the network it is on with no GCP knowledge in the visualizer.
func (s *Server) instanceParent(body map[string]any) string {
	nics, _ := body["networkInterfaces"].([]any)
	for _, n := range nics {
		if nic, ok := n.(map[string]any); ok {
			if sub := str(nic["subnetwork"]); sub != "" {
				return sub
			}
			if net := str(nic["network"]); net != "" {
				return net
			}
		}
	}
	return s.projectSelfLink()
}

// networkOfSubnetwork reads a stored subnetwork's network link.
func (s *Server) networkOfSubnetwork(link string) string {
	res, err := s.store.Get(link)
	if err != nil {
		return ""
	}
	var sub struct {
		Network string `json:"network"`
	}
	if json.Unmarshal(res.Body, &sub) != nil {
		return ""
	}
	return sub.Network
}

// metadataValue reads one key out of the items array.
func metadataValue(body map[string]any, key string) string {
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		return ""
	}
	items, _ := meta["items"].([]any)
	for _, it := range items {
		if item, ok := it.(map[string]any); ok && str(item["key"]) == key {
			return str(item["value"])
		}
	}
	return ""
}

// sourceImageOf reads the boot disk's image reference.
func sourceImageOf(body map[string]any) string {
	disks, _ := body["disks"].([]any)
	for _, d := range disks {
		disk, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if params, ok := disk["initializeParams"].(map[string]any); ok {
			if img := str(params["sourceImage"]); img != "" {
				return img
			}
		}
		if img := str(disk["sourceImage"]); img != "" {
			return img
		}
	}
	return ""
}

// bootInstance starts the container, then finishes the operation and marks
// the instance RUNNING.
//
// Under s.vmMu for its whole length: a delete that arrives meanwhile must
// not be overtaken, or a destroy reports success and leaves a container
// running. That is what the GKE node pool's rescale paid for.
func (s *Server) bootInstance(link, opLink string, body map[string]any) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	if _, err := s.store.Get(link); err != nil {
		return // deleted before it finished booting
	}

	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()

	name := str(body["name"])
	image := metadataValue(body, instanceImageKey)
	if image == "" {
		image = dockerImageFor(sourceImageOf(body))
	}

	var keys []string
	// GCE's ssh-keys metadata is "user:key" per line, because one instance
	// can have keys for several users. Only the key itself is useful here.
	for _, line := range strings.Split(metadataValue(body, "ssh-keys"), "\n") {
		if _, key, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && key != "" {
			keys = append(keys, key)
		}
	}

	m := &vm.Instance{
		ResourceID: link,
		Name:       name,
		Image:      image,
		UserData:   metadataValue(body, "startup-script"),
		PublicKeys: keys,
		Metadata:   s.instanceMetadata(link),
		MetadataEnv: func(_, hostPort string) map[string]string {
			// GCE_METADATA_HOST is host:port with NO scheme, where AWS's
			// endpoint variable is a full URL. The plugin decides, which is
			// why the engine hands over both forms.
			return map[string]string{"GCE_METADATA_HOST": hostPort}
		},
	}
	if err := s.vms.Start(ctx, m); err != nil {
		log.Printf("nimbus: instance %s failed to start: %v", name, err)
		s.setInstanceStatus(link, "TERMINATED", "")
		s.lros.fail(opLink, err.Error())
		s.operations.finish(opLink)
		return
	}
	s.setInstanceStatus(link, "RUNNING", m.Container)
	s.operations.finish(opLink)
}

// setInstanceStatus moves the status along, through Store.Update so the
// read, the change and the write are one step.
func (s *Server) setInstanceStatus(link, status, container string) {
	_ = s.store.Update(link, func(body map[string]any) error {
		body["status"] = status
		if container != "" {
			// Recorded so `nimbus ssh` -- a separate process, which cannot
			// see the engine -- can find the container. The same field name
			// AWS uses, so that command needs no per-cloud table.
			body["nimbusContainer"] = container
		}
		return nil
	})
}

func (s *Server) getInstance(w http.ResponseWriter, scope, name string) string {
	link := s.instanceLink(scope, name)
	res, err := s.store.Get(link)
	if err != nil {
		writeError(w, notFound("The resource '%s' was not found", link))
		return "no such instance"
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
	return ""
}

func (s *Server) listInstances(w http.ResponseWriter, scope string) string {
	prefix := s.instanceLink(scope, "")
	items := []any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Cloud == "gcp" && r.Type == TypeInstance && strings.HasPrefix(r.ID, prefix)
	}) {
		var body map[string]any
		if json.Unmarshal(res.Body, &body) == nil {
			items = append(items, body)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":     "compute#instanceList",
		"id":       "projects/" + s.cfg.ProjectID + "/zones/" + strings.TrimPrefix(scope, "zones/") + "/instances",
		"items":    items,
		"selfLink": s.instanceLink(scope, ""),
	})
	return ""
}

func (s *Server) deleteInstance(w http.ResponseWriter, scope, name string) string {
	link := s.instanceLink(scope, name)
	if _, err := s.store.Get(link); err != nil {
		writeError(w, notFound("The resource '%s' was not found", link))
		return "no such instance"
	}
	// The container is removed SYNCHRONOUSLY, before the operation is
	// answered, and the operation is DONE when it is.
	//
	// Measured, because the obvious shape was wrong: doing it in the
	// background and answering a pending operation made `terraform
	// destroy` return while the container was still being removed --
	// reproducibly, one container still up the instant destroy exited and
	// gone three seconds later. A destroy that reports success with a
	// container still running is exactly the failure the GKE node pool
	// taught, so there is no version of this worth leaving asynchronous.
	//
	// The create has to be asynchronous because it pulls an image. A delete
	// has nothing slow in it: `docker rm -f` takes well under a second.
	s.deleteInstanceContainer(link)
	_, _ = s.store.Delete(link)
	writeJSON(w, http.StatusOK, s.newOperation(scope, "delete", link))
	return "instance " + name + " deleted"
}

// deleteInstanceContainer removes the container and waits for it. Under
// s.vmMu, so a boot still in flight cannot start one behind the delete.
func (s *Server) deleteInstanceContainer(link string) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()
	if m := s.vms.Get(link); m != nil {
		if err := s.vms.Delete(ctx, m); err != nil {
			log.Printf("nimbus: deleting instance container: %v", err)
		}
	}
}

// instanceVerb is stop, start and reset: trailing path segments, not colon
// verbs. GKE's setSize is a colon verb and Pub/Sub's publish is too, which
// makes this the third convention in one cloud.
func (s *Server) instanceVerb(w http.ResponseWriter, scope, name, verb string) string {
	link := s.instanceLink(scope, name)
	if _, err := s.store.Get(link); err != nil {
		writeError(w, notFound("The resource '%s' was not found", link))
		return "no such instance"
	}
	switch verb {
	case "stop", "start", "reset":
	default:
		writeError(w, notFound("Nimbus does not implement the instance verb %q yet", verb))
		return "instance verb " + verb
	}

	// STOPPING and STAGING are the states GCE reports while it happens.
	switch verb {
	case "stop":
		s.setInstanceStatus(link, "STOPPING", "")
	default:
		s.setInstanceStatus(link, "STAGING", "")
	}
	op := s.newPendingOperation(scope, verb, link)
	go s.applyInstanceVerb(link, verb, op.SelfLink)
	writeJSON(w, http.StatusOK, op)
	return "instance " + name + " " + verb
}

func (s *Server) applyInstanceVerb(link, verb, opLink string) {
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	defer s.operations.finish(opLink)

	ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
	defer cancel()

	m := s.vms.Get(link)
	if m == nil {
		return
	}
	switch verb {
	case "stop":
		if err := s.vms.Stop(ctx, m); err != nil {
			log.Printf("nimbus: stopping instance: %v", err)
		}
		s.setInstanceStatus(link, "TERMINATED", "")
	case "start":
		if err := s.vms.Resume(ctx, m); err != nil {
			log.Printf("nimbus: starting instance: %v", err)
			s.setInstanceStatus(link, "TERMINATED", "")
			return
		}
		s.setInstanceStatus(link, "RUNNING", m.Container)
	case "reset":
		if err := s.vms.Restart(ctx, m); err != nil {
			log.Printf("nimbus: resetting instance: %v", err)
		}
		s.setInstanceStatus(link, "RUNNING", m.Container)
	}
}

// internalIPFor derives an address from the instance's link, so it is the
// same on every read -- a value that differed would be a permanent diff,
// the Content-MD5 lesson again. 10.128.0.0/9 is GCE's default auto-mode
// range, which is what a reader expects to see.
func internalIPFor(link string) string {
	sum := sha256.Sum256([]byte("nimbus/gcp/ip/" + link))
	// .2 upwards: GCE reserves the first addresses of a subnet, and .1 is
	// the gateway.
	return fmt.Sprintf("10.128.%d.%d", int(sum[0])%256, 2+int(sum[1])%250)
}

// fingerprintOf is a short DERIVED token. Compute sends a fingerprint back
// on every update to detect a concurrent write; nothing here does optimistic
// concurrency, so there is nothing to lose by deriving it and a stable read
// to gain -- the same reasoning as Azure's etags.
func fingerprintOf(seed string) string {
	sum := sha256.Sum256([]byte("nimbus/gcp/fingerprint/" + seed))
	return base64.StdEncoding.EncodeToString(sum[:8])
}

// serveAggregatedInstances answers `GET
// /compute/v1/projects/{p}/aggregated/instances`, which is how **gcloud**
// lists instances -- across every zone at once, rather than the zonal
// collection the Terraform provider uses:
//
//	gcloud compute instances list
//	  -> /compute/v1/projects/nimbus-local/aggregated/instances
//	     ?includeAllScopes=True&maxResults=500
//
// Two clients, two paths, one resource. The same split `gcloud run` and the
// Cloud Run provider have, and worth knowing before assuming a collection
// is served just because one client can read it.
//
// The answer is a MAP keyed by scope, not a list: items["zones/{z}"]
// carries that zone's instances. A client reading items as an array finds
// nothing.
func (s *Server) serveAggregatedInstances(w http.ResponseWriter, r *http.Request) string {
	if r.Method != http.MethodGet {
		writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
		return "aggregated instances " + r.Method
	}

	byZone := map[string][]any{}
	for _, res := range s.store.List(func(res *core.Resource) bool {
		return res.Cloud == "gcp" && res.Type == TypeInstance
	}) {
		var body map[string]any
		if json.Unmarshal(res.Body, &body) != nil {
			continue
		}
		zone := lastSegment(str(body["zone"]))
		if zone == "" {
			zone = res.Location
		}
		byZone["zones/"+zone] = append(byZone["zones/"+zone], body)
	}

	items := map[string]any{}
	for scope, instances := range byZone {
		items[scope] = map[string]any{"instances": instances}
	}
	// A scope with nothing in it carries a warning rather than an empty
	// list, which is what real Compute answers and what gcloud prints as
	// "Listed 0 items".
	if len(items) == 0 {
		items["zones/"+s.cfg.Zone] = map[string]any{
			"warning": map[string]any{
				"code":    "NO_RESULTS_ON_PAGE",
				"message": "There are no results for scope 'zones/" + s.cfg.Zone + "' on this page.",
			},
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":     "compute#instanceAggregatedList",
		"id":       "projects/" + s.cfg.ProjectID + "/aggregated/instances",
		"items":    items,
		"selfLink": s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/aggregated/instances",
	})
	return ""
}
