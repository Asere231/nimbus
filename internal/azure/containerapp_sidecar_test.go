package azure

import (
	"testing"
)

// A template's second container becomes a sidecar, not a note in the state.
//
// Real Container Apps run every container in the template as a group
// sharing one network namespace, so they reach each other on localhost.
// Nimbus ran the first and stored the rest -- so an app with a cache or a
// log shipper beside it applied cleanly, reported Succeeded, and behaved as
// though half its configuration was not there. Worse than an error, because
// nothing said so.
func TestEveryContainerInTheTemplateBecomesASidecar(t *testing.T) {
	props := map[string]any{
		"template": map[string]any{
			"containers": []any{
				map[string]any{
					"name":  "web",
					"image": "nginx:alpine",
					"env": []any{
						map[string]any{"name": "MAIN", "value": "yes"},
					},
				},
				map[string]any{
					"name":  "cache",
					"image": "redis:alpine",
					"env": []any{
						map[string]any{"name": "REDIS_PORT", "value": "6379"},
					},
				},
				map[string]any{
					"name":    "shipper",
					"image":   "busybox:latest",
					"command": []any{"/bin/sh"},
					"args":    []any{"-c", "sleep 3600"},
				},
			},
		},
	}

	if got := containerAppImage(props); got != "nginx:alpine" {
		t.Errorf("the main image should still be the first container's, got %q", got)
	}

	sidecars := containerAppSidecars(props)
	if len(sidecars) != 2 {
		t.Fatalf("expected two sidecars, got %d: %+v", len(sidecars), sidecars)
	}
	if sidecars[0].Name != "cache" || sidecars[0].Image != "redis:alpine" {
		t.Errorf("first sidecar is wrong: %+v", sidecars[0])
	}
	if sidecars[0].Env["REDIS_PORT"] != "6379" {
		t.Errorf("a sidecar's environment was dropped: %+v", sidecars[0].Env)
	}
	// The main container's environment must not leak into a sidecar, nor
	// the other way: they are separate containers with separate configs.
	if _, leaked := sidecars[0].Env["MAIN"]; leaked {
		t.Errorf("the main container's environment leaked into a sidecar: %+v", sidecars[0].Env)
	}
	if sidecars[1].Name != "shipper" {
		t.Fatalf("second sidecar is wrong: %+v", sidecars[1])
	}
	if len(sidecars[1].Command) != 1 || sidecars[1].Command[0] != "/bin/sh" {
		t.Errorf("a sidecar's command was dropped: %+v", sidecars[1].Command)
	}
	if len(sidecars[1].Args) != 2 || sidecars[1].Args[1] != "sleep 3600" {
		t.Errorf("a sidecar's args were dropped: %+v", sidecars[1].Args)
	}
}

// A single-container app has no sidecars, which is every existing example.
func TestASingleContainerAppHasNoSidecars(t *testing.T) {
	props := map[string]any{
		"template": map[string]any{
			"containers": []any{map[string]any{"name": "web", "image": "nginx:alpine"}},
		},
	}
	if got := containerAppSidecars(props); len(got) != 0 {
		t.Errorf("expected no sidecars, got %+v", got)
	}
}

// A container with no image is SKIPPED rather than started as an empty
// name, which docker would refuse with an error naming nothing useful.
func TestAContainerWithNoImageIsNotASidecar(t *testing.T) {
	props := map[string]any{
		"template": map[string]any{
			"containers": []any{
				map[string]any{"name": "web", "image": "nginx:alpine"},
				map[string]any{"name": "broken"},
			},
		},
	}
	if got := containerAppSidecars(props); len(got) != 0 {
		t.Errorf("a container with no image should not become a sidecar: %+v", got)
	}
}

// A sidecar with no name of its own still gets one, because the container
// name is derived from it and two unnamed sidecars must not collide.
func TestAnUnnamedSidecarGetsAPositionalName(t *testing.T) {
	props := map[string]any{
		"template": map[string]any{
			"containers": []any{
				map[string]any{"image": "nginx:alpine"},
				map[string]any{"image": "redis:alpine"},
				map[string]any{"image": "busybox:latest"},
			},
		},
	}
	got := containerAppSidecars(props)
	if len(got) != 2 {
		t.Fatalf("expected two sidecars, got %+v", got)
	}
	if got[0].Name == "" || got[1].Name == "" {
		t.Fatalf("an unnamed sidecar got no name: %+v", got)
	}
	if got[0].Name == got[1].Name {
		t.Errorf("two unnamed sidecars share the name %q, so their containers would collide",
			got[0].Name)
	}
}

// minReplicas is honoured: the count in the template is the number of
// container groups that run.
//
// It used to round-trip as state with nothing acting on it, so a
// configuration asking for three replicas applied cleanly and ran one --
// the same silent shape sidecars had.
func TestMinReplicasIsTheReplicaCount(t *testing.T) {
	cases := []struct {
		scale any
		want  int
	}{
		{nil, 1},
		{map[string]any{}, 1},
		{map[string]any{"minReplicas": float64(0)}, 1},
		{map[string]any{"minReplicas": float64(3)}, 3},
		{map[string]any{"minReplicas": float64(3), "maxReplicas": float64(10)}, 3},
		// An int rather than a float64, which is what a value set in Go
		// rather than decoded from JSON looks like -- the trap GCP's node
		// count already fell into.
		{map[string]any{"minReplicas": 2}, 2},
	}
	for _, c := range cases {
		props := map[string]any{"template": map[string]any{
			"containers": []any{map[string]any{"name": "web", "image": "nginx:alpine"}},
		}}
		if c.scale != nil {
			props["template"].(map[string]any)["scale"] = c.scale
		}
		if got := containerAppReplicas(props); got != c.want {
			t.Errorf("scale %v gave %d replicas, want %d", c.scale, got, c.want)
		}
	}
}

// maxReplicas alone changes nothing, and that is deliberate: scaling OUT
// means reacting to load and nothing here measures load. Honouring a
// maximum as though it were a minimum would run containers nobody asked
// for.
func TestMaxReplicasAloneRunsOne(t *testing.T) {
	props := map[string]any{"template": map[string]any{
		"containers": []any{map[string]any{"name": "web", "image": "nginx:alpine"}},
		"scale":      map[string]any{"maxReplicas": float64(10)},
	}}
	if got := containerAppReplicas(props); got != 1 {
		t.Errorf("maxReplicas alone gave %d replicas, want 1", got)
	}
}
