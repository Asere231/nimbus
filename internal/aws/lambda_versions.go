package aws

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Published versions and aliases: a function's history, and the stable
// names that point into it.
//
// Lambda had only $LATEST. `aws_lambda_function` with `publish = true` and
// `aws_lambda_alias` -- both ordinary provider resources -- could not
// apply, and the roadmap's reason for leaving it ("nothing a local apply
// does needs it") was a claim about the emulator rather than about the
// clients.
//
// # What a published version IS
//
// An immutable SNAPSHOT of the configuration AND the code, taken at the
// moment of publishing, addressed by a qualified ARN
// (`...:function:name:3`). $LATEST goes on changing underneath it. So the
// code has to be COPIED at publish: leaving the version pointing at the
// live directory would make `publish` a label rather than a snapshot --
// and invoking version 1 would run version 2's handler, which is the
// silent-wrong-answer shape this project refuses everywhere else.
//
// # The container is started LAZILY, on the first invoke
//
// Real Lambda keeps every published version independently invocable. Doing
// that eagerly here would start a container per publish, and
// `publish = true` publishes on every apply -- so a handful of applies
// would quietly leave a dozen containers running for versions nothing ever
// calls. Nothing observes a version's container until something invokes
// it: the provider reads `version` and `qualified_arn` out of
// PublishVersion's own response and never waits on a version's State. So
// the first invoke of a version pays for starting it, and the image is
// already local by then because $LATEST pulled it.
//
// # An alias may not look like a version
//
// A qualifier is EITHER a version number or an alias name, and both make
// the same qualified ARN, so an alias called "1" would be a second
// resource with the first one's id. Real AWS forbids it for the same
// reason; Nimbus refuses it by name rather than letting one overwrite the
// other.

const (
	typeLambdaVersion = "AWS::Lambda::Version"
	typeLambdaAlias   = "AWS::Lambda::Alias"
)

// latestQualifier is the unpublished version every function always has.
const latestQualifier = "$LATEST"

// lambdaVersionARN is a qualified ARN: the function's, a colon, and the
// qualifier. The same shape for a version number and an alias name, which
// is why an alias may not be numeric.
func (s *Server) lambdaVersionARN(name, qualifier string) string {
	return s.lambdaARN(name) + ":" + qualifier
}

// lambdaAlias is the stored shape, field names from the wire.
type lambdaAlias struct {
	AliasArn        string         `json:"AliasArn"`
	Name            string         `json:"Name"`
	FunctionVersion string         `json:"FunctionVersion"`
	Description     string         `json:"Description,omitempty"`
	RevisionId      string         `json:"RevisionId"`
	RoutingConfig   map[string]any `json:"RoutingConfig,omitempty"`
}

// --- publishing -----------------------------------------------------------

// publishVersion answers POST /2015-03-31/functions/{name}/versions.
//
// Note that this is NOT how `publish = true` creates version 1. Measured
// against aws provider 5.100.0: the provider sends `"Publish": true` in
// the CreateFunction BODY and never calls this path for a create -- so
// honouring the flag there is what makes the common configuration work,
// and implementing only this endpoint left `publish = true` reporting
// version `$LATEST` with no version created and nothing erroring. See
// publishOnCreate.
func (s *Server) publishVersion(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	var req struct {
		Description string `json:"Description"`
		CodeSha256  string `json:"CodeSha256"`
		RevisionId  string `json:"RevisionId"`
	}
	// An empty body is legal here, so a parse failure is not fatal.
	_ = readJSON(r, &req)

	// Real Lambda refuses to publish when CodeSha256 is given and does not
	// match what is deployed -- it is how a client makes sure it is
	// publishing the code it just uploaded rather than somebody else's.
	if req.CodeSha256 != "" && req.CodeSha256 != fn.CodeSha256 {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "CodeSha256 " + req.CodeSha256 + " does not match the deployed code (" +
				fn.CodeSha256 + ")"}
	}

	snapshot, aerr := s.snapshotVersion(fn, req.Description, nil)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 201, snapshot)
	return nil
}

// snapshotVersion records an immutable copy of a function as the next
// version number, and returns it.
//
// `prepare` fills the version's CODE directory. It is nil for the
// ordinary publish, which COPIES the live directory -- but on a create
// the code has not been unpacked yet (that happens in the background
// goroutine that starts the container), so the create passes a prepare
// that unpacks the zip it already holds. Copying from a directory that
// does not exist yet would make version 1 of every `publish = true`
// function empty, and only on a create, which is the hardest case to
// notice.
func (s *Server) snapshotVersion(fn *lambdaFunction, description string,
	prepare func(dir string) error) (*lambdaFunction, *apiError) {

	number := strconv.Itoa(s.nextLambdaVersion(fn.FunctionName))
	snapshot := *fn
	snapshot.Version = number
	snapshot.FunctionArn = s.lambdaVersionARN(fn.FunctionName, number)
	snapshot.Description = orDefault(description, fn.Description)
	snapshot.LastModified = lambdaTime()
	snapshot.RevisionId = uuidLike()
	// A published version is immediately Active: there is nothing to wait
	// for, because the code is already unpacked and the image already
	// local. The container comes up on the first invoke -- see the note at
	// the top of this file.
	snapshot.State = "Active"
	snapshot.LastUpdateStatus = "Successful"
	// The image is RECORDED rather than re-derived. For a Zip function it
	// follows from Runtime, but a PackageType: Image function's ImageUri
	// is not stored anywhere, so deriving would work for most functions
	// and silently fail for those.
	if image, err := lambdaImage(fn.Runtime, ""); err == nil {
		snapshot.NimbusImage = image
	}
	if running := s.lambda.Get(fn.FunctionArn); running != nil && running.Image != "" {
		snapshot.NimbusImage = running.Image
	}
	// Invocations are per version, so a snapshot does not inherit the
	// live function's count.
	snapshot.NimbusInvocations = 0

	// The code is COPIED, which is what makes the version a snapshot. If
	// it cannot be written the publish FAILS rather than producing a
	// version that would run whatever $LATEST becomes.
	dir := s.lambda.CodeDir(snapshot.FunctionArn)
	var err error
	if prepare != nil {
		err = prepare(dir)
	} else {
		err = copyTree(s.lambda.CodeDir(fn.FunctionArn), dir)
	}
	if err != nil {
		return nil, &apiError{Status: 500, Code: "ServiceException",
			Message: "storing the code for version " + number + ": " + err.Error()}
	}

	body, err := json.Marshal(snapshot)
	if err != nil {
		return nil, &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud:    "aws",
		ID:       snapshot.FunctionArn,
		Type:     typeLambdaVersion,
		Name:     fn.FunctionName + ":" + number,
		ParentID: fn.FunctionArn,
		Body:     body,
	})
	return &snapshot, nil
}

// publishOnCreate handles `"Publish": true` in a CreateFunction or
// UpdateFunctionCode body, which is how the aws provider's
// `publish = true` really works -- it never POSTs to .../versions.
//
// The returned number is what the response's Version field must say: the
// provider reads `version` and `qualified_arn` straight out of that
// response and never looks again, so a version published and not
// REPORTED is a permanent diff. Same rule as the GKE endpoint that has to
// be filled by the create.
// The stored function's own Version stays "$LATEST", and only the
// RESPONSE says otherwise. That distinction matters twice: an unqualified
// GetFunctionConfiguration must keep reporting $LATEST, as real Lambda
// does, and the invoke path decides whether to start a version's
// container by asking whether what it resolved is $LATEST -- so a live
// function claiming to be version 1 would send an ordinary invoke looking
// for a snapshot's container.
func (s *Server) publishOnCreate(fn *lambdaFunction, code []byte) string {
	snapshot, aerr := s.snapshotVersion(fn, "", func(dir string) error {
		return funcs.Unpack(code, dir)
	})
	if aerr != nil {
		// The function itself is fine; only the version failed. Saying so
		// beats failing a create that otherwise worked.
		log.Printf("nimbus: could not publish a version of %s: %s", fn.FunctionName, aerr.Message)
		return ""
	}
	return snapshot.Version
}

// nextLambdaVersion is one past the highest ever published.
//
// Derived from what the store HOLDS, which is safe here in a way it was
// not for a Cloud Storage generation: Lambda never deletes a version
// implicitly, and a version a client deleted on purpose is one real Lambda
// also never reissues -- but a client cannot ask about a version by
// number it has not been given, so reusing a number after an explicit
// delete cannot mislead anything. The monotonic property that matters is
// that a LIVE version's number is never reassigned.
func (s *Server) nextLambdaVersion(name string) int {
	highest := 0
	for _, v := range s.lambdaVersions(name) {
		if n, err := strconv.Atoi(v.Version); err == nil && n > highest {
			highest = n
		}
	}
	return highest + 1
}

// lambdaVersions is every published version of a function, in numeric
// order -- not the lexical order the store's ids would give, where 10
// sorts before 2.
func (s *Server) lambdaVersions(name string) []*lambdaFunction {
	prefix := s.lambdaARN(name) + ":"
	var out []*lambdaFunction
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == typeLambdaVersion && strings.HasPrefix(r.ID, prefix)
	}) {
		var v lambdaFunction
		if json.Unmarshal(res.Body, &v) == nil {
			out = append(out, &v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(out[i].Version)
		b, _ := strconv.Atoi(out[j].Version)
		return a < b
	})
	return out
}

// listVersionsByFunction answers GET .../versions: $LATEST first, then the
// published versions in numeric order, which is the order real Lambda
// reports.
func (s *Server) listVersionsByFunction(w http.ResponseWriter, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	versions := []*lambdaFunction{fn}
	versions = append(versions, s.lambdaVersions(fn.FunctionName)...)
	writeJSON(w, 200, map[string]any{"Versions": versions})
	return nil
}

// --- aliases --------------------------------------------------------------

func (s *Server) lambdaAliasARN(name, alias string) string {
	return s.lambdaVersionARN(name, alias)
}

func (s *Server) loadAlias(name, alias string) (*lambdaAlias, *apiError) {
	res, err := s.store.Get(s.lambdaAliasARN(name, alias))
	if err != nil || res.Type != typeLambdaAlias {
		return nil, &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: "Alias not found: " + s.lambdaAliasARN(name, alias)}
	}
	var a lambdaAlias
	if err := json.Unmarshal(res.Body, &a); err != nil {
		return nil, &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	return &a, nil
}

func (s *Server) createAlias(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	var req lambdaAlias
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	if req.Name == "" {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "Name is required"}
	}
	if aerr := checkAliasName(req.Name); aerr != nil {
		return aerr
	}
	if req.FunctionVersion == "" {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "FunctionVersion is required"}
	}
	if aerr := s.checkVersionExists(fn.FunctionName, req.FunctionVersion); aerr != nil {
		return aerr
	}
	if s.store.Exists(s.lambdaAliasARN(fn.FunctionName, req.Name)) {
		return &apiError{Status: 409, Code: "ResourceConflictException",
			Message: "Alias already exists: " + req.Name}
	}
	alias := req
	alias.AliasArn = s.lambdaAliasARN(fn.FunctionName, req.Name)
	alias.RevisionId = uuidLike()
	if aerr := s.saveAlias(fn, &alias); aerr != nil {
		return aerr
	}
	writeJSON(w, 201, alias)
	return nil
}

func (s *Server) getAlias(w http.ResponseWriter, name, alias string) *apiError {
	a, aerr := s.loadAlias(lambdaName(name), alias)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, a)
	return nil
}

func (s *Server) updateAlias(w http.ResponseWriter, r *http.Request, name, alias string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	existing, aerr := s.loadAlias(fn.FunctionName, alias)
	if aerr != nil {
		return aerr
	}
	var req lambdaAlias
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	// A field the client left out keeps its value: this is a partial
	// update, and clearing what was not mentioned would drop a
	// description the provider still has in its configuration.
	if req.FunctionVersion != "" {
		if aerr := s.checkVersionExists(fn.FunctionName, req.FunctionVersion); aerr != nil {
			return aerr
		}
		existing.FunctionVersion = req.FunctionVersion
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.RoutingConfig != nil {
		existing.RoutingConfig = req.RoutingConfig
	}
	existing.RevisionId = uuidLike()
	if aerr := s.saveAlias(fn, existing); aerr != nil {
		return aerr
	}
	writeJSON(w, 200, existing)
	return nil
}

func (s *Server) deleteAlias(w http.ResponseWriter, name, alias string) *apiError {
	if _, err := s.store.Delete(s.lambdaAliasARN(lambdaName(name), alias)); err != nil {
		// Deleting an alias that is not there SUCCEEDS, which is what
		// makes a destroy idempotent -- the same rule as an object.
		w.WriteHeader(204)
		return nil
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) listAliases(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	// FunctionVersion narrows the listing to the aliases pointing at one
	// version, which is how a client finds what would break if it
	// deleted that version.
	wanted := r.URL.Query().Get("FunctionVersion")
	prefix := s.lambdaARN(fn.FunctionName) + ":"
	out := []*lambdaAlias{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == typeLambdaAlias && strings.HasPrefix(r.ID, prefix)
	}) {
		var a lambdaAlias
		if json.Unmarshal(res.Body, &a) != nil {
			continue
		}
		if wanted != "" && a.FunctionVersion != wanted {
			continue
		}
		out = append(out, &a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"Aliases": out})
	return nil
}

func (s *Server) saveAlias(fn *lambdaFunction, a *lambdaAlias) *apiError {
	body, err := json.Marshal(a)
	if err != nil {
		return &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud:    "aws",
		ID:       a.AliasArn,
		Type:     typeLambdaAlias,
		Name:     fn.FunctionName + ":" + a.Name,
		ParentID: fn.FunctionArn,
		Body:     body,
	})
	return nil
}

// checkAliasName refuses a name that would collide with a version number.
//
// A qualified ARN cannot say which kind of qualifier it carries, so an
// alias called "1" and version 1 are the same id. Real AWS forbids a
// numeric alias name for exactly this reason, and refusing by name is
// better than letting one silently replace the other.
func checkAliasName(name string) *apiError {
	if name == latestQualifier {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "An alias cannot be called " + latestQualifier}
	}
	if _, err := strconv.Atoi(name); err == nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "Alias name " + name + " is numeric, which cannot be told from a " +
				"version number in a qualified ARN"}
	}
	return nil
}

// checkVersionExists refuses an alias pointing at a version nobody
// published. Real Lambda refuses it too, and the alternative is an alias
// that resolves to nothing at invoke time, far from the mistake.
func (s *Server) checkVersionExists(name, version string) *apiError {
	if version == latestQualifier {
		return nil
	}
	if s.store.Exists(s.lambdaVersionARN(name, version)) {
		return nil
	}
	return &apiError{Status: 404, Code: "ResourceNotFoundException",
		Message: "Function not found: " + s.lambdaVersionARN(name, version)}
}

// --- resolving a qualifier ------------------------------------------------

// lambdaQualifier is the qualifier a request names, from the `Qualifier`
// query parameter or from a `name:qualifier` reference. Empty means
// $LATEST, which is what an unqualified call asks for.
func lambdaQualifier(ref string, r *http.Request) string {
	if q := r.URL.Query().Get("Qualifier"); q != "" {
		return q
	}
	if decoded, err := url.PathUnescape(ref); err == nil {
		ref = decoded
	}
	// A bare name has no colon; an ARN has several, and the qualifier is
	// only a qualifier when it follows the function name -- the eighth
	// field of a seven-colon ARN.
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		if strings.Count(ref, ":") >= 7 || !strings.HasPrefix(ref, "arn:") {
			return ref[i+1:]
		}
	}
	return ""
}

// resolveQualified is the function a qualified request is about: the live
// one for $LATEST or no qualifier, the snapshot for a version number, and
// whatever an alias points at for an alias.
func (s *Server) resolveQualified(name, qualifier string) (*lambdaFunction, *apiError) {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return nil, aerr
	}
	if qualifier == "" || qualifier == latestQualifier {
		return fn, nil
	}
	if _, err := strconv.Atoi(qualifier); err != nil {
		// An alias, so follow it to a version -- and an alias pointing at
		// $LATEST resolves to the live function, which real Lambda allows.
		a, aerr := s.loadAlias(fn.FunctionName, qualifier)
		if aerr != nil {
			return nil, aerr
		}
		qualifier = pickAliasVersion(a, rand.Float64())
		if qualifier == latestQualifier {
			return fn, nil
		}
	}
	res, err := s.store.Get(s.lambdaVersionARN(fn.FunctionName, qualifier))
	if err != nil || res.Type != typeLambdaVersion {
		return nil, &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: "Function not found: " + s.lambdaVersionARN(fn.FunctionName, qualifier)}
	}
	var v lambdaFunction
	if err := json.Unmarshal(res.Body, &v); err != nil {
		return nil, &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	return &v, nil
}

// ensureVersionRunning starts a published version's container if nothing
// is running for it yet, and waits. See the note at the top of this file
// for why this is not done at publish time.
func (s *Server) ensureVersionRunning(fn *lambdaFunction) *apiError {
	if s.lambda.Get(fn.FunctionArn) != nil {
		return nil
	}
	if fn.NimbusImage == "" {
		return &apiError{Status: 409, Code: "ResourceNotReadyException",
			Message: "Version " + fn.Version + " of " + fn.FunctionName +
				" records no image, so Nimbus cannot start it."}
	}
	command := []string{fn.Handler}
	if fn.Handler == "" {
		command = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	running := &funcs.Function{
		ResourceID: fn.FunctionArn,
		Name:       fn.FunctionName + "-" + fn.Version,
		Image:      fn.NimbusImage,
		Command:    command,
		Dir:        s.lambda.CodeDir(fn.FunctionArn),
		Env:        s.lambdaEnv(fn),
	}
	if err := s.lambda.Start(ctx, running); err != nil {
		return &apiError{Status: 409, Code: "ResourceNotReadyException",
			Message: "Version " + fn.Version + " of " + fn.FunctionName +
				" could not be started: " + err.Error()}
	}
	return nil
}

// stopLambdaQualifiers stops and forgets everything a function's versions
// left running, and removes their code.
//
// Called when the function goes. A version's container is parented to the
// function in the STORE, so the cascade removes the records -- but a
// container and a code directory are not store resources and nothing
// would reach them. The same rule that found the AKS node resource group:
// anything created as a side effect has to be removed by the thing that
// made it.
func (s *Server) stopLambdaQualifiers(ctx context.Context, name string) {
	for _, v := range s.lambdaVersions(name) {
		if running := s.lambda.Get(v.FunctionArn); running != nil {
			s.lambda.Stop(ctx, running)
		}
		s.lambda.RemoveCode(v.FunctionArn)
	}
}

// --- copying the code -----------------------------------------------------

// copyTree copies a directory, replacing the destination.
//
// The MODE travels, because a custom runtime's `bootstrap` has to stay
// executable -- the same reason Unpack preserves it, and an `exec format
// error` at invoke time is all a lost bit would say. Windows has no
// executable bit at all, so there the mode drives only the read-only flag
// and nothing is lost: the bit matters inside the Linux container.
func copyTree(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		return err
	}
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// A symlink is NOT followed: a zip entry can name one, and copying
		// what it points at would pull in a file from outside the code
		// directory. Skipped rather than refused, because the unpack that
		// wrote it already decided what was safe to keep.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		return err
	})
}

// pickAliasVersion is which version an invoke through an alias really
// reaches, honouring a weighted routing config.
//
// `routing_config.additional_version_weights` is how a canary is
// expressed: `{"2": 0.1}` sends a tenth of the traffic to version 2 and
// the rest to the version the alias names. HONOURED rather than recorded,
// because an ignored weight is not a missing feature -- it is every
// request going one way while the configuration says otherwise, which a
// client reads as an answer about its canary. The same argument that makes
// an unparsed DynamoDB filter a refusal rather than a match-everything.
//
// `roll` is passed in rather than drawn here so the choice is a pure
// function of the config and one number, which is the only way to test a
// split without counting thousands of invocations.
//
// Note what this cannot be: weighted routing is RANDOM per request, so two
// invokes of one alias legitimately answer from different versions. Real
// Lambda is the same, and it is why X-Amz-Executed-Version names the
// version that really ran rather than the alias.
func pickAliasVersion(a *lambdaAlias, roll float64) string {
	weights, _ := a.RoutingConfig["AdditionalVersionWeights"].(map[string]any)
	if len(weights) == 0 {
		return a.FunctionVersion
	}
	// A stable order, so one number always picks the same version: a map's
	// iteration order would make the same roll land differently each time.
	others := make([]string, 0, len(weights))
	for version := range weights {
		others = append(others, version)
	}
	sort.Strings(others)

	cumulative := 0.0
	for _, version := range others {
		weight, ok := weights[version].(float64)
		if !ok || weight <= 0 {
			continue
		}
		cumulative += weight
		if roll < cumulative {
			return version
		}
	}
	// Everything not claimed by a weight goes to the version the alias
	// names, which is what makes the weights ADDITIONAL.
	return a.FunctionVersion
}
