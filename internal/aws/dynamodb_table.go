package aws

import (
	"encoding/json"
	"net/http"

	"github.com/Asere231/nimbus/internal/core"
)

// Tables, and the four separate calls a client makes about one.
//
// A table is a core.Resource; its items are data in the item store. The
// shape DescribeTable returns is the shape that was stored, because the
// provider reads every field of it back on the next refresh -- the same
// "derive, do not generate" rule that fixes a perpetual diff everywhere
// else in this project.
//
// Tags, time-to-live and point-in-time recovery each have their own call
// and their own "nothing is configured" answer, which the provider reads
// straight after a create. This is S3's dozen bucket sub-resources again,
// one service later: a refusal on any of them fails the apply.

// --- tables ---------------------------------------------------------------

// dynamoTable is the stored shape, which is also what DescribeTable
// returns. The field names are the wire's, capitalised, because they are
// echoed back verbatim.
type dynamoTable struct {
	TableName        string           `json:"TableName"`
	TableArn         string           `json:"TableArn"`
	TableId          string           `json:"TableId"`
	TableStatus      string           `json:"TableStatus"`
	CreationDateTime float64          `json:"CreationDateTime"`
	AttributeDefs    []map[string]any `json:"AttributeDefinitions"`
	KeySchema        []map[string]any `json:"KeySchema"`
	ItemCount        int              `json:"ItemCount"`
	TableSizeBytes   int              `json:"TableSizeBytes"`

	BillingModeSummary     map[string]any   `json:"BillingModeSummary,omitempty"`
	ProvisionedThroughput  map[string]any   `json:"ProvisionedThroughput"`
	GlobalSecondaryIndexes []map[string]any `json:"GlobalSecondaryIndexes,omitempty"`
	LocalSecondaryIndexes  []map[string]any `json:"LocalSecondaryIndexes,omitempty"`
	StreamSpecification    map[string]any   `json:"StreamSpecification,omitempty"`
	SSEDescription         map[string]any   `json:"SSEDescription,omitempty"`
	TableClassSummary      map[string]any   `json:"TableClassSummary,omitempty"`
	DeletionProtection     bool             `json:"DeletionProtectionEnabled"`

	// Kept beside the table rather than returned by DescribeTable, because
	// that is where the real API keeps them: each has its own call. The
	// nimbus* names are so a client reading a table body in the visualizer
	// can see they are ours and not DynamoDB's.
	NimbusTags []map[string]any `json:"nimbusTags,omitempty"`
	NimbusTTL  map[string]any   `json:"nimbusTtl,omitempty"`
	NimbusPITR bool             `json:"nimbusPitr,omitempty"`
}

func (s *Server) loadDynamoTable(req map[string]any) (*dynamoTable, *apiError) {
	name := str(req["TableName"])
	if name == "" {
		return nil, dynamoBadRequest("TableName is required")
	}
	res, err := s.store.Get(s.dynamoTableARN(name))
	if err != nil || res.Type != typeDynamoTable {
		return nil, dynamoNotFound(name)
	}
	var t dynamoTable
	if err := json.Unmarshal(res.Body, &t); err != nil {
		return nil, &apiError{Status: 500, Code: "InternalServerError", Message: err.Error()}
	}
	return &t, nil
}

func (s *Server) saveDynamoTable(t *dynamoTable) *apiError {
	body, err := json.Marshal(t)
	if err != nil {
		return &apiError{Status: 500, Code: "InternalServerError", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: t.TableArn, Type: typeDynamoTable, Name: t.TableName,
		ParentID: s.accountARN(), Location: s.cfg.Region, Backing: "real", Body: body,
	})
	return nil
}

func (s *Server) createDynamoTable(w http.ResponseWriter, req map[string]any) *apiError {
	name := str(req["TableName"])
	if name == "" {
		return dynamoBadRequest("TableName is required")
	}
	if s.store.Exists(s.dynamoTableARN(name)) {
		return &apiError{Status: 400, Code: "ResourceInUseException",
			Message: "Table already exists: " + name}
	}
	keySchema := objList(req["KeySchema"])
	if len(keySchema) == 0 {
		return dynamoBadRequest("KeySchema is required")
	}

	billing := str(req["BillingMode"])
	if billing == "" {
		billing = "PROVISIONED"
	}
	t := &dynamoTable{
		TableName: name,
		TableArn:  s.dynamoTableARN(name),
		// Decided once and stored: an id derived from the name would come
		// back the same for a table deleted and recreated, which is a
		// different table.
		TableId:          uuidLike(),
		TableStatus:      "ACTIVE", // nothing has to be provisioned, so it is ready
		CreationDateTime: epoch(),
		AttributeDefs:    objList(req["AttributeDefinitions"]),
		KeySchema:        keySchema,
		BillingModeSummary: map[string]any{
			"BillingMode": billing, "LastUpdateToPayPerRequestDateTime": epoch(),
		},
		// PAY_PER_REQUEST still reports a throughput, of zeroes. The
		// provider reads the field either way.
		ProvisionedThroughput:  throughputOf(req["ProvisionedThroughput"]),
		GlobalSecondaryIndexes: indexDescriptions(objList(req["GlobalSecondaryIndexes"]), "GSI"),
		LocalSecondaryIndexes:  indexDescriptions(objList(req["LocalSecondaryIndexes"]), "LSI"),
		NimbusTags:             objList(req["Tags"]),
		DeletionProtection:     boolOf(req["DeletionProtectionEnabled"]),
	}
	if spec, ok := req["StreamSpecification"].(map[string]any); ok && boolOf(spec["StreamEnabled"]) {
		t.StreamSpecification = spec
	}
	if sse, ok := req["SSESpecification"].(map[string]any); ok && boolOf(sse["Enabled"]) {
		t.SSEDescription = map[string]any{"Status": "ENABLED", "SSEType": "KMS"}
	}
	if tc, ok := req["TableClass"].(string); ok && tc != "" {
		t.TableClassSummary = map[string]any{"TableClass": tc}
	}

	if aerr := s.saveDynamoTable(t); aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{"TableDescription": s.withCounts(t)})
	return nil
}

func (s *Server) describeDynamoTable(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{"Table": s.withCounts(t)})
	return nil
}

// withCounts is a table on its way out: the two fields that are about the
// DATA rather than the table are counted at read time instead of being kept
// in step by every write, and the fields Nimbus keeps for itself are
// dropped, because they are not DynamoDB's and do not belong in a response.
func (s *Server) withCounts(t *dynamoTable) *dynamoTable {
	out := *t
	out.ItemCount, out.TableSizeBytes = s.items.stats(t.TableName)
	out.NimbusTags, out.NimbusTTL, out.NimbusPITR = nil, nil, false
	return &out
}

func (s *Server) listDynamoTables(w http.ResponseWriter) *apiError {
	names := []string{}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeDynamoTable }) {
		names = append(names, res.Name)
	}
	writeJSON(w, 200, map[string]any{"TableNames": names})
	return nil
}

func (s *Server) deleteDynamoTable(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	if t.DeletionProtection {
		return &apiError{Status: 400, Code: "ValidationException",
			Message: "Table cannot be deleted while DeletionProtectionEnabled is set to True"}
	}
	s.items.dropTable(t.TableName)
	_, _ = s.store.Delete(t.TableArn)
	t.TableStatus = "DELETING"
	writeJSON(w, 200, map[string]any{"TableDescription": s.withCounts(t)})
	return nil
}

func (s *Server) updateDynamoTable(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	if billing := str(req["BillingMode"]); billing != "" {
		t.BillingModeSummary = map[string]any{
			"BillingMode": billing, "LastUpdateToPayPerRequestDateTime": epoch(),
		}
	}
	if _, ok := req["ProvisionedThroughput"]; ok {
		t.ProvisionedThroughput = throughputOf(req["ProvisionedThroughput"])
	}
	if defs := objList(req["AttributeDefinitions"]); len(defs) > 0 {
		t.AttributeDefs = defs
	}
	if _, ok := req["DeletionProtectionEnabled"]; ok {
		t.DeletionProtection = boolOf(req["DeletionProtectionEnabled"])
	}
	if spec, ok := req["StreamSpecification"].(map[string]any); ok {
		if boolOf(spec["StreamEnabled"]) {
			t.StreamSpecification = spec
		} else {
			t.StreamSpecification = nil
		}
	}
	if tc := str(req["TableClass"]); tc != "" {
		t.TableClassSummary = map[string]any{"TableClass": tc}
	}
	// A GSI is added or dropped through a list of single-key updates.
	for _, u := range objList(req["GlobalSecondaryIndexUpdates"]) {
		if create, ok := u["Create"].(map[string]any); ok {
			t.GlobalSecondaryIndexes = append(t.GlobalSecondaryIndexes,
				indexDescriptions([]map[string]any{create}, "GSI")...)
		}
		if del, ok := u["Delete"].(map[string]any); ok {
			name := str(del["IndexName"])
			kept := t.GlobalSecondaryIndexes[:0]
			for _, ix := range t.GlobalSecondaryIndexes {
				if str(ix["IndexName"]) != name {
					kept = append(kept, ix)
				}
			}
			t.GlobalSecondaryIndexes = kept
		}
	}
	if aerr := s.saveDynamoTable(t); aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{"TableDescription": s.withCounts(t)})
	return nil
}

// --- the things read back after a create ----------------------------------

func (s *Server) listDynamoTags(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(map[string]any{
		"TableName": tableFromARN(str(req["ResourceArn"])),
	})
	if aerr != nil {
		return aerr
	}
	tags := t.NimbusTags
	if tags == nil {
		tags = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"Tags": tags})
	return nil
}

func (s *Server) tagDynamoResource(w http.ResponseWriter, req map[string]any, add bool) *apiError {
	name := tableFromARN(str(req["ResourceArn"]))
	t, aerr := s.loadDynamoTable(map[string]any{"TableName": name})
	if aerr != nil {
		return aerr
	}
	if add {
		for _, tag := range objList(req["Tags"]) {
			t.NimbusTags = upsertTag(t.NimbusTags, tag)
		}
	} else {
		for _, key := range strList(req["TagKeys"]) {
			kept := []map[string]any{}
			for _, tag := range t.NimbusTags {
				if str(tag["Key"]) != key {
					kept = append(kept, tag)
				}
			}
			t.NimbusTags = kept
		}
	}
	if aerr := s.saveDynamoTable(t); aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{})
	return nil
}

func upsertTag(tags []map[string]any, tag map[string]any) []map[string]any {
	for i, have := range tags {
		if str(have["Key"]) == str(tag["Key"]) {
			tags[i] = tag
			return tags
		}
	}
	return append(tags, tag)
}

// Point-in-time recovery is off unless something turned it on, and the
// answer is a document rather than an error -- the provider reads it on
// every refresh.
func (s *Server) describeContinuousBackups(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	status := "DISABLED"
	if t.NimbusPITR {
		status = "ENABLED"
	}
	writeJSON(w, 200, map[string]any{"ContinuousBackupsDescription": map[string]any{
		"ContinuousBackupsStatus": "ENABLED",
		"PointInTimeRecoveryDescription": map[string]any{
			"PointInTimeRecoveryStatus": status,
		},
	}})
	return nil
}

func (s *Server) updateContinuousBackups(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	if spec, ok := req["PointInTimeRecoverySpecification"].(map[string]any); ok {
		t.NimbusPITR = boolOf(spec["PointInTimeRecoveryEnabled"])
	}
	if aerr := s.saveDynamoTable(t); aerr != nil {
		return aerr
	}
	return s.describeContinuousBackups(w, req)
}

func (s *Server) describeTimeToLive(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	desc := map[string]any{"TimeToLiveStatus": "DISABLED"}
	if t.NimbusTTL != nil {
		desc = t.NimbusTTL
	}
	writeJSON(w, 200, map[string]any{"TimeToLiveDescription": desc})
	return nil
}

func (s *Server) updateTimeToLive(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	spec, _ := req["TimeToLiveSpecification"].(map[string]any)
	status := "DISABLED"
	if boolOf(spec["Enabled"]) {
		status = "ENABLED"
	}
	t.NimbusTTL = map[string]any{
		"TimeToLiveStatus": status, "AttributeName": str(spec["AttributeName"]),
	}
	if aerr := s.saveDynamoTable(t); aerr != nil {
		return aerr
	}
	writeJSON(w, 200, map[string]any{"TimeToLiveSpecification": spec})
	return nil
}
