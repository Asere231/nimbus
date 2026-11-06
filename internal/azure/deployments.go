package azure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/azure/deployments"
)

// Template deployments.
//
// A deployment is an ordinary ARM resource whose body carries a template. What
// makes it interesting is what happens next: every resource in the template is
// created by PUTting it back through this same server, so hooks run, the store
// is updated and the visualizer draws each one appearing. Nothing about a
// deployed resource is special afterwards.
//
// `az deployment group create --template-file x.bicep` never sends Bicep: the
// CLI compiles it locally and sends ARM JSON, so there is no Bicep parser here
// and there does not need to be.

const typeDeployment = "Microsoft.Resources/deployments"

func registerDeploymentHooks(s *Server) {
	s.registerHook(typeDeployment, Hook{
		// A template that cannot be expanded is rejected at submit time, as
		// ARM does with InvalidTemplate, rather than accepted and then failed
		// asynchronously: the client gets the reason in the response it is
		// already reading.
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props, _ := body["properties"].(map[string]any)
			if props == nil {
				return &armError{400, "InvalidDeployment", "A deployment needs a properties section."}
			}
			template, aerr := templateOf(props)
			if aerr != nil {
				return aerr
			}
			if _, err := s.expand(t, template, props); err != nil {
				return &armError{400, "InvalidTemplate", err.Error()}
			}
			if props["mode"] == nil {
				props["mode"] = "Incremental"
			}
			props["timestamp"] = time.Now().UTC().Format(time.RFC3339)
			props["correlationId"] = stableGUID(t.ID + "/correlation")
			props["duration"] = "PT0.1S"
			return nil
		},
		// Deploying is long-running for real: each resource is created one
		// after another, and a client polls meanwhile.
		Provision: func(s *Server, t target, body map[string]any) error {
			return s.runDeployment(t, body)
		},
	})
}

// templateOf pulls the template out of a deployment's properties. ARM accepts
// it inline; templateLink (a URL) is deliberately not supported, because
// fetching it would mean leaving the machine.
func templateOf(props map[string]any) (map[string]any, *armError) {
	if _, linked := props["templateLink"]; linked {
		if props["template"] == nil {
			return nil, &armError{400, "InvalidTemplate",
				"Nimbus deploys inline templates only: templateLink would have to be fetched over the network."}
		}
	}
	template, ok := props["template"].(map[string]any)
	if !ok {
		return nil, &armError{400, "InvalidTemplate", "properties.template must be an ARM template object."}
	}
	return template, nil
}

// scopeFor builds the expression scope a template is evaluated in.
func (s *Server) scopeFor(t target) *deployments.Scope {
	return &deployments.Scope{
		SubscriptionID:  t.Subscription,
		ResourceGroup:   t.ResourceGroup,
		ResourceGroupID: "/subscriptions/" + t.Subscription + "/resourceGroups/" + t.ResourceGroup,
		Location:        s.groupLocation(t),
		TenantID:        s.cfg.TenantID,
		DeploymentName:  t.Name,
		Reference:       s.referenceResource,
	}
}

// groupLocation answers resourceGroup().location, which templates use for
// nearly every resource they create.
func (s *Server) groupLocation(t target) string {
	res, err := s.store.Get("/subscriptions/" + t.Subscription + "/resourceGroups/" + t.ResourceGroup)
	if err != nil {
		return "eastus"
	}
	if res.Location != "" {
		return res.Location
	}
	return "eastus"
}

// referenceResource is what reference() resolves against: the store, which is
// the same thing a client would read.
func (s *Server) referenceResource(id string) (map[string]any, bool) {
	res, err := s.store.Get(id)
	if err != nil {
		return nil, false
	}
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return nil, false
	}
	return body, true
}

func (s *Server) expand(t target, template map[string]any, props map[string]any) (*deployments.Plan, error) {
	params, _ := props["parameters"].(map[string]any)
	return deployments.Expand(template, params, s.scopeFor(t))
}

// runDeployment creates every resource in the template, in dependency order,
// then records the operations and evaluates the outputs.
func (s *Server) runDeployment(t target, body map[string]any) error {
	props, _ := body["properties"].(map[string]any)
	template, aerr := templateOf(props)
	if aerr != nil {
		return fmt.Errorf("%s", aerr.Message)
	}
	plan, err := s.expand(t, template, props)
	if err != nil {
		return err
	}

	scope := s.scopeFor(t)
	ops := make([]any, 0, len(plan.Resources))
	providers := map[string]map[string]bool{}

	for i, r := range plan.Resources {
		// Re-evaluate now: expressions that needed reference() could not be
		// resolved when the plan was built, but the resources they name have
		// been created by the time we reach the one that uses them.
		resolved, err := deployments.EvaluateDeep(r.Body, scope)
		if err != nil {
			return fmt.Errorf("%s %s: %w", r.Type, r.Name, err)
		}
		rb, _ := resolved.(map[string]any)
		if rb == nil {
			rb = map[string]any{}
		}
		status, raw, err := s.putInternal(r.ID, r.APIVersion, rb)
		if err != nil {
			return fmt.Errorf("%s %s: %w", r.Type, r.Name, err)
		}
		if status >= 400 {
			return fmt.Errorf("%s %s: %s", r.Type, r.Name, armMessage(raw))
		}
		ops = append(ops, deploymentOperation(t, r, i, status))

		ns, rest, _ := strings.Cut(r.Type, "/")
		if providers[ns] == nil {
			providers[ns] = map[string]bool{}
		}
		providers[ns][rest] = true
	}

	outputs, err := plan.EvaluateOutputs(scope)
	if err != nil {
		return err
	}

	// Write the results onto the deployment itself. The provisioningState is
	// set by the generic provisioning path once this returns.
	if err := s.store.Update(t.ID, func(stored map[string]any) error {
		sp, _ := stored["properties"].(map[string]any)
		if sp == nil {
			sp = map[string]any{}
		}
		sp["outputs"] = outputs
		sp["parameters"] = wrapParameters(plan.Parameters)
		sp["providers"] = providerList(providers)
		sp["dependencies"] = []any{}
		sp["outputResources"] = outputResources(plan)
		stored["properties"] = sp
		return nil
	}); err != nil {
		return err
	}

	s.deploymentOps.set(t.ID, ops)
	return nil
}

// putInternal creates one resource by running it through the ARM handler, the
// same path an HTTP client takes. Going through the real handler rather than
// the store directly is what makes a deployed resource indistinguishable from
// one Terraform created: the same hooks fire and the same events reach the
// visualizer.
func (s *Server) putInternal(id, apiVersion string, body map[string]any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	if apiVersion == "" {
		apiVersion = "2021-04-01"
	}
	req, err := http.NewRequest(http.MethodPut, s.cfg.PublicURL+id+"?api-version="+apiVersion, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	rec := &capture{header: http.Header{}, status: 200}
	s.handleARM(rec, req)
	return rec.status, rec.body.Bytes(), nil
}

// capture is an http.ResponseWriter that keeps the response in memory, for
// requests Nimbus makes to itself.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.header }
func (c *capture) WriteHeader(code int)        { c.status = code }
func (c *capture) Write(p []byte) (int, error) { return c.body.Write(p) }

func armMessage(raw []byte) string {
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Error.Message != "" {
		return out.Error.Code + ": " + out.Error.Message
	}
	return string(raw)
}

func deploymentOperation(t target, r deployments.Resource, i, status int) map[string]any {
	return map[string]any{
		"id":          t.ID + "/operations/" + fmt.Sprintf("%08X", i),
		"operationId": fmt.Sprintf("%08X", i),
		"properties": map[string]any{
			"provisioningOperation": "Create",
			"provisioningState":     "Succeeded",
			"timestamp":             time.Now().UTC().Format(time.RFC3339),
			"duration":              "PT0.1S",
			"statusCode":            http.StatusText(status),
			"targetResource": map[string]any{
				"id":           r.ID,
				"resourceType": r.Type,
				"resourceName": r.Name,
			},
		},
	}
}

func wrapParameters(params map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range params {
		out[k] = map[string]any{"type": paramType(v), "value": v}
	}
	return out
}

func paramType(v any) string {
	switch v.(type) {
	case bool:
		return "Bool"
	case float64:
		return "Int"
	case []any:
		return "Array"
	case map[string]any:
		return "Object"
	}
	return "String"
}

func providerList(providers map[string]map[string]bool) []any {
	out := []any{}
	for ns, types := range providers {
		list := []any{}
		for typ := range types {
			list = append(list, map[string]any{"resourceType": typ, "locations": []any{nil}})
		}
		out = append(out, map[string]any{"namespace": ns, "resourceTypes": list})
	}
	return out
}

func outputResources(plan *deployments.Plan) []any {
	out := []any{}
	for _, r := range plan.Resources {
		out = append(out, map[string]any{"id": r.ID})
	}
	return out
}

// --- actions ---------------------------------------------------------------

// deploymentAction handles validate, whatIf and the other POSTs a deployment
// accepts. Neither validate nor what-if may create anything.
func (s *Server) deploymentAction(w http.ResponseWriter, t target, body map[string]any) bool {
	// An action path parses with the action appended to the type:
	// ".../deployments/dep1/validate" gives the type
	// "Microsoft.Resources/deployments/validate".
	if !eq(t.Type, typeDeployment+"/"+t.Action) {
		return false
	}
	switch strings.ToLower(t.Action) {
	case "validate":
		props, _ := body["properties"].(map[string]any)
		if props == nil {
			writeError(w, 400, "InvalidDeployment", "A deployment needs a properties section.")
			return true
		}
		template, aerr := templateOf(props)
		if aerr != nil {
			writeError(w, aerr.Status, aerr.Code, "%s", aerr.Message)
			return true
		}
		plan, err := s.expand(t, template, props)
		if err != nil {
			writeError(w, 400, "InvalidTemplate", "%v", err)
			return true
		}
		writeJSON(w, 200, map[string]any{
			"id":   t.ID,
			"name": t.Name,
			"properties": map[string]any{
				"provisioningState": "Succeeded",
				"mode":              props["mode"],
				"parameters":        wrapParameters(plan.Parameters),
				"validatedResources": func() []any {
					out := []any{}
					for _, r := range plan.Resources {
						out = append(out, map[string]any{"id": r.ID})
					}
					return out
				}(),
			},
		})
		return true

	case "whatif":
		props, _ := body["properties"].(map[string]any)
		if props == nil {
			writeError(w, 400, "InvalidDeployment", "A deployment needs a properties section.")
			return true
		}
		template, aerr := templateOf(props)
		if aerr != nil {
			writeError(w, aerr.Status, aerr.Code, "%s", aerr.Message)
			return true
		}
		plan, err := s.expand(t, template, props)
		if err != nil {
			writeError(w, 400, "InvalidTemplate", "%v", err)
			return true
		}
		changes := []any{}
		for _, r := range plan.Resources {
			change := map[string]any{"resourceId": r.ID, "changeType": "Create"}
			if existing, ok := s.referenceResource(r.ID); ok {
				// Comparing the whole body would report a change for every
				// server-filled field, so only what the template states is
				// compared -- which is also all the template can change.
				change["changeType"] = "NoChange"
				if differs(r.Body, existing) {
					change["changeType"] = "Modify"
				}
				change["before"] = existing
			}
			change["after"] = r.Body
			changes = append(changes, change)
		}
		writeJSON(w, 200, map[string]any{
			"status": "Succeeded",
			"properties": map[string]any{
				"changes": changes,
			},
			"changes": changes,
		})
		return true

	case "cancel":
		writeJSON(w, 200, map[string]any{})
		return true

	case "exporttemplate":
		res, err := s.store.Get(t.ID)
		if err != nil {
			writeError(w, 404, "DeploymentNotFound", "Deployment '%s' could not be found.", t.Name)
			return true
		}
		var stored map[string]any
		_ = json.Unmarshal(res.Body, &stored)
		props, _ := stored["properties"].(map[string]any)
		writeJSON(w, 200, map[string]any{"template": props["template"]})
		return true
	}
	return false
}

// differs reports whether anything the template states disagrees with what is
// stored. Only what the template mentions is compared -- which is also all a
// template can change -- because the stored resource has server-filled fields
// the template never mentions and never will.
//
// That subset rule has to reach inside arrays too. A security rule in a
// template is {name, properties{...}}, while the stored one has also gained
// an id, an etag and a provisioningState; comparing the arrays whole reports
// Modify for a resource nothing has touched.
func differs(want, have map[string]any) bool {
	for k, wv := range want {
		if strings.EqualFold(k, "name") || strings.EqualFold(k, "id") || strings.EqualFold(k, "type") {
			continue
		}
		hv, ok := lookupFold(have, k)
		if !ok {
			return true
		}
		if valueDiffers(wv, hv) {
			return true
		}
	}
	return false
}

func valueDiffers(want, have any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return true
		}
		return differs(w, h)
	case []any:
		h, ok := have.([]any)
		if !ok || len(w) != len(h) {
			return true
		}
		for i := range w {
			if valueDiffers(w[i], h[i]) {
				return true
			}
		}
		return false
	}
	wraw, _ := json.Marshal(want)
	hraw, _ := json.Marshal(have)
	return string(wraw) != string(hraw)
}

func lookupFold(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// --- operations ------------------------------------------------------------

// deploymentOperations remembers what each deployment did, which clients list
// to show progress and to explain a failure.
type deploymentOperations struct {
	mu  sync.RWMutex
	ops map[string][]any
}

func newDeploymentOperations() *deploymentOperations {
	return &deploymentOperations{ops: map[string][]any{}}
}

func (d *deploymentOperations) set(id string, ops []any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ops[strings.ToLower(id)] = ops
}

func (d *deploymentOperations) get(id string) []any {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.ops[strings.ToLower(id)]
}

// isDeploymentOperationsPath matches .../deployments/{name}/operations, which
// parses as a collection whose action is "operations".
func isDeploymentOperationsPath(t target) bool {
	return t.Kind == "collection" && eq(t.Type, typeDeployment+"/operations") &&
		strings.EqualFold(t.Action, "operations")
}

func (s *Server) handleDeploymentOperations(w http.ResponseWriter, t target) string {
	ops := s.deploymentOps.get(t.ID)
	if ops == nil {
		ops = []any{}
	}
	writeJSON(w, 200, map[string]any{"value": ops})
	return ""
}
