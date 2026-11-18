package gcp

import (
	"testing"
)

// Cloud Run runs every container in the template too.
//
// The engine gained sidecars for Azure Container Apps, and Cloud Run
// shares that engine -- so leaving GCP unwired would mean one cloud had
// the feature and the other did not, for no reason but the wiring. Cloud
// Run's own shape differs from Azure's only in spelling: the containers
// live under template.containers either way, and its env is the same list
// of {name, value}.
func TestCloudRunReadsEveryContainerAsASidecar(t *testing.T) {
	body := map[string]any{
		"template": map[string]any{
			"containers": []any{
				map[string]any{"name": "main", "image": "gcr.io/x/web"},
				map[string]any{
					"name":  "cache",
					"image": "redis:alpine",
					"env": []any{
						map[string]any{"name": "REDIS_PORT", "value": "6379"},
					},
				},
				map[string]any{
					"name":    "shipper",
					"image":   "busybox",
					"command": []any{"/bin/sh"},
					"args":    []any{"-c", "sleep 1"},
				},
			},
		},
	}

	if got := firstImage(body); got != "gcr.io/x/web" {
		t.Errorf("the main image should still be the first container's, got %q", got)
	}
	sidecars := serviceSidecars(body)
	if len(sidecars) != 2 {
		t.Fatalf("expected two sidecars, got %d: %+v", len(sidecars), sidecars)
	}
	if sidecars[0].Name != "cache" || sidecars[0].Image != "redis:alpine" {
		t.Errorf("first sidecar wrong: %+v", sidecars[0])
	}
	if sidecars[0].Env["REDIS_PORT"] != "6379" {
		t.Errorf("a sidecar's environment was dropped: %+v", sidecars[0].Env)
	}
	if len(sidecars[1].Command) != 1 || sidecars[1].Args[1] != "sleep 1" {
		t.Errorf("a sidecar's command or args were dropped: %+v", sidecars[1])
	}
}

// A one-container service has no sidecars, which is every existing
// example and must not change.
func TestASingleContainerServiceHasNoSidecars(t *testing.T) {
	body := map[string]any{"template": map[string]any{
		"containers": []any{map[string]any{"name": "main", "image": "gcr.io/x/web"}},
	}}
	if got := serviceSidecars(body); len(got) != 0 {
		t.Errorf("expected no sidecars, got %+v", got)
	}
}

// A template with no containers at all yields nothing rather than panicking.
func TestNoContainersYieldsNoSidecars(t *testing.T) {
	if got := serviceSidecars(map[string]any{}); len(got) != 0 {
		t.Errorf("expected no sidecars from an empty body, got %+v", got)
	}
}
