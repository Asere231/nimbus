package gcp

import (
	"net/http"
	"strings"
)

// Public images.
//
// The google provider RESOLVES an image before it creates an instance,
// which is the opposite of what the aws provider does -- that one sends an
// AMI id to RunInstances and never asks what it means. Measured against
// google 6.50.0, creating one google_compute_instance:
//
//	GET /compute/v1/projects/debian-cloud/global/images/debian-12
//	GET /compute/v1/projects/debian-cloud/global/images/family/debian-12
//
// Two things in those paths matter. The project is **debian-cloud**, not
// ours: public images live in well-known projects, so this has to answer
// for a project nobody created. And the provider tries the image by name
// and by FAMILY, because `debian-cloud/debian-12` is ambiguous between the
// two -- a family resolves to whatever its latest image is.
//
// An image is a catalogue entry, not a resource anyone created, so nothing
// here is stored. It is answered from the name.
//
// That name is also what decides the container image, and unlike an AMI id
// it can: `debian-12` and `ubuntu-2404-lts` say what they are. So where an
// EC2 instance needs a nimbus:image tag to know what to run, a GCE one
// usually needs nothing.

// DefaultInstanceImage backs an image name nothing else matches. Alpine
// because it is tiny and its busybox has the base64 and tee the vm
// engine's bootstrap needs.
const DefaultInstanceImage = "alpine:3.20"

// imageAliases maps the recognisable part of a public image or family name
// to a Docker image. Longest match wins, so "ubuntu-2404" beats "ubuntu".
//
// These are real base images, so a startup script that runs apt or apk is
// running the real thing. They are not GCE's images: there is no kernel, no
// cloud-init and no guest environment, which is why the list is short and
// honest rather than an attempt at a catalogue.
var imageAliases = []struct{ prefix, image string }{
	{"debian-13", "debian:13"},
	{"debian-12", "debian:12"},
	{"debian-11", "debian:11"},
	{"debian", "debian:12"},
	{"ubuntu-2404", "ubuntu:24.04"},
	{"ubuntu-2204", "ubuntu:22.04"},
	{"ubuntu-2004", "ubuntu:20.04"},
	{"ubuntu", "ubuntu:24.04"},
	{"rocky-linux-9", "rockylinux:9"},
	{"alpine", "alpine:3.20"},
	{"cos", "alpine:3.20"}, // Container-Optimized OS: no equivalent, and nothing needs one
}

// dockerImageFor turns a GCE image or family reference into a container
// image. It accepts anything the provider might send: a bare name, a
// family, "project/name", or a full self link.
func dockerImageFor(ref string) string {
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	for _, a := range imageAliases {
		if strings.HasPrefix(name, a.prefix) {
			return a.image
		}
	}
	return DefaultInstanceImage
}

// serveImages answers a public image lookup, by name or by family.
//
// rest is what follows `global/images`: either {name} or family/{family}.
func (s *Server) serveImages(w http.ResponseWriter, r *http.Request, project string, rest []string) string {
	if r.Method != http.MethodGet || len(rest) == 0 {
		writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
		return "compute images"
	}

	name, family := rest[0], ""
	if rest[0] == "family" {
		if len(rest) < 2 {
			writeError(w, badRequest("an image family lookup needs a family name"))
			return "compute image family"
		}
		family, name = rest[1], rest[1]
	}

	// The self link names the project it was asked for, because that is
	// what the provider puts in the disk's sourceImage and then reads back.
	link := s.base() + "/compute/v1/projects/" + project + "/global/images/" + name
	body := map[string]any{
		"kind":     "compute#image",
		"id":       numericID(link),
		"name":     name,
		"selfLink": link,
		"status":   "READY",
		// Decimal strings, as every Compute number is.
		"diskSizeGb":        "10",
		"archiveSizeBytes":  "1073741824",
		"sourceType":        "RAW",
		"architecture":      "X86_64",
		"creationTimestamp": timestamp(),
		"description":       "Nimbus stands in for " + name + " with " + dockerImageFor(name),
		"labels":            map[string]any{},
		"licenses":          []any{},
		"guestOsFeatures": []any{
			map[string]any{"type": "UEFI_COMPATIBLE"},
			map[string]any{"type": "VIRTIO_SCSI_MULTIQUEUE"},
		},
	}
	if family != "" {
		body["family"] = family
	}
	writeJSON(w, http.StatusOK, body)
	return "compute image " + name
}
