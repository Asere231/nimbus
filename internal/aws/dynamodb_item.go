package aws

import (
	"encoding/json"
	"net/http"
	"sort"
)

// The item operations.
//
// An item is a map of attribute values, stored and returned in exactly the
// shape it arrived in. The only thing Nimbus computes about an item is its
// KEY, which is how the store addresses it -- and a key is derived from the
// table's own KeySchema, not guessed from the attributes present.

// keyNames is the table's partition key and, if it has one, its sort key.
func keyNames(t *dynamoTable) (hash, rangeKey string) {
	for _, el := range t.KeySchema {
		switch str(el["KeyType"]) {
		case "HASH":
			hash = str(el["AttributeName"])
		case "RANGE":
			rangeKey = str(el["AttributeName"])
		}
	}
	return hash, rangeKey
}

// itemKey addresses an item in the store.
//
// The JSON of the key attributes, in schema order, rather than anything
// prettier: it has to distinguish {"N":"5"} from {"S":"5"}, which are
// different keys in DynamoDB and would collide under any encoding that
// threw the type away.
func itemKey(t *dynamoTable, item map[string]any) (string, *apiError) {
	hash, rangeKey := keyNames(t)
	parts := make([]any, 0, 2)
	for _, name := range []string{hash, rangeKey} {
		if name == "" {
			continue
		}
		v, ok := item[name]
		if !ok {
			return "", dynamoBadRequest(
				"One of the required keys was not given a value: %s", name)
		}
		parts = append(parts, map[string]any{name: v})
	}
	body, err := json.Marshal(parts)
	if err != nil {
		return "", dynamoBadRequest("unusable key: %v", err)
	}
	return string(body), nil
}

// exprNames and exprValues are the two substitution maps every expression
// call carries.
func exprNames(req map[string]any) map[string]string {
	out := map[string]string{}
	raw, _ := req["ExpressionAttributeNames"].(map[string]any)
	for alias, name := range raw {
		out[alias] = str(name)
	}
	return out
}

func exprValues(req map[string]any) map[string]any {
	out := map[string]any{}
	raw, _ := req["ExpressionAttributeValues"].(map[string]any)
	for placeholder, value := range raw {
		out[placeholder] = value
	}
	return out
}

// checkCondition evaluates a ConditionExpression against what is there now.
// A failure is its own exception, which is what makes "write only if absent"
// work for a client.
func checkCondition(req map[string]any, current map[string]any) *apiError {
	expr := str(req["ConditionExpression"])
	if expr == "" {
		return nil
	}
	cond, aerr := parseCondition(expr, exprNames(req), exprValues(req))
	if aerr != nil {
		return aerr
	}
	if current == nil {
		current = map[string]any{}
	}
	if !cond.matches(current) {
		return &apiError{Status: 400, Code: "ConditionalCheckFailedException",
			Message: "The conditional request failed"}
	}
	return nil
}

func (s *Server) putItem(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	item, ok := req["Item"].(map[string]any)
	if !ok {
		return dynamoBadRequest("Item is required")
	}
	key, aerr := itemKey(t, item)
	if aerr != nil {
		return aerr
	}
	if aerr := checkCondition(req, s.items.get(t.TableName, key)); aerr != nil {
		return aerr
	}
	old := s.items.put(t.TableName, key, item)

	out := map[string]any{}
	if str(req["ReturnValues"]) == "ALL_OLD" && old != nil {
		out["Attributes"] = old
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getItem(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	keyAttrs, ok := req["Key"].(map[string]any)
	if !ok {
		return dynamoBadRequest("Key is required")
	}
	key, aerr := itemKey(t, keyAttrs)
	if aerr != nil {
		return aerr
	}
	item := s.items.get(t.TableName, key)
	out := map[string]any{}
	if item != nil {
		// A missing item is an EMPTY response, not an error: that is how a
		// client asks "is this there?" without handling an exception.
		projected, aerr := projection(item, str(req["ProjectionExpression"]), exprNames(req))
		if aerr != nil {
			return aerr
		}
		out["Item"] = projected
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) updateItem(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	keyAttrs, ok := req["Key"].(map[string]any)
	if !ok {
		return dynamoBadRequest("Key is required")
	}
	key, aerr := itemKey(t, keyAttrs)
	if aerr != nil {
		return aerr
	}
	current := s.items.get(t.TableName, key)
	if aerr := checkCondition(req, current); aerr != nil {
		return aerr
	}

	// An update to an item that is not there CREATES it, carrying the key.
	// A client counting on that is the common case: ADD n :one on a counter
	// nobody initialised.
	old := map[string]any{}
	updated := map[string]any{}
	for name, v := range current {
		old[name] = v
		updated[name] = v
	}
	for name, v := range keyAttrs {
		updated[name] = v
	}

	expr := str(req["UpdateExpression"])
	if expr == "" {
		return dynamoBadRequest(
			"Nimbus requires an UpdateExpression; the older AttributeUpdates form is not implemented")
	}
	if aerr := applyUpdate(updated, expr, exprNames(req), exprValues(req)); aerr != nil {
		return aerr
	}
	s.items.put(t.TableName, key, updated)

	out := map[string]any{}
	switch str(req["ReturnValues"]) {
	case "ALL_NEW":
		out["Attributes"] = updated
	case "ALL_OLD":
		if current != nil {
			out["Attributes"] = old
		}
	case "UPDATED_NEW":
		out["Attributes"] = changedAttributes(old, updated)
	case "UPDATED_OLD":
		out["Attributes"] = changedAttributes(updated, old)
	}
	writeJSON(w, 200, out)
	return nil
}

// changedAttributes is what UPDATED_NEW and UPDATED_OLD mean: only the
// attributes that differ between the two versions.
func changedAttributes(before, after map[string]any) map[string]any {
	out := map[string]any{}
	for name, v := range after {
		if !avEqual(before[name], v) {
			out[name] = v
		}
	}
	return out
}

func (s *Server) deleteItem(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	keyAttrs, ok := req["Key"].(map[string]any)
	if !ok {
		return dynamoBadRequest("Key is required")
	}
	key, aerr := itemKey(t, keyAttrs)
	if aerr != nil {
		return aerr
	}
	current := s.items.get(t.TableName, key)
	if aerr := checkCondition(req, current); aerr != nil {
		return aerr
	}
	// Deleting an item that is not there SUCCEEDS, which is what makes a
	// destroy idempotent -- the same rule S3 objects follow.
	old := s.items.delete(t.TableName, key)
	out := map[string]any{}
	if str(req["ReturnValues"]) == "ALL_OLD" && old != nil {
		out["Attributes"] = old
	}
	writeJSON(w, 200, out)
	return nil
}

// --- reading many ---------------------------------------------------------

// queryItems answers a Query: the key condition first, then the filter.
//
// An IndexName needs no separate storage. A secondary index's keys are
// ordinary attributes of the same items, so the key condition selects on
// them directly and the sort key orders them -- the one thing this does not
// model is a projection type, so an index query can return attributes a
// KEYS_ONLY index would have omitted. Returning too much is the direction
// to be wrong in: a client narrows what it asked for, and nothing breaks.
func (s *Server) queryItems(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	keyExpr := str(req["KeyConditionExpression"])
	if keyExpr == "" {
		return dynamoBadRequest(
			"Nimbus requires a KeyConditionExpression; the older KeyConditions form is not implemented")
	}
	keyCond, aerr := parseCondition(keyExpr, exprNames(req), exprValues(req))
	if aerr != nil {
		return aerr
	}
	filter, aerr := parseCondition(str(req["FilterExpression"]), exprNames(req), exprValues(req))
	if aerr != nil {
		return aerr
	}

	var matched []map[string]any
	scanned := 0
	for _, item := range s.items.all(t.TableName) {
		if !keyCond.matches(item) {
			continue
		}
		scanned++
		if filter.matches(item) {
			matched = append(matched, item)
		}
	}

	// Within a partition, items come back in sort-key order; ascending
	// unless the client says otherwise.
	sortKey := queryRangeKey(t, req)
	if sortKey != "" {
		forward := true
		if v, ok := req["ScanIndexForward"].(bool); ok {
			forward = v
		}
		sort.SliceStable(matched, func(i, j int) bool {
			cmp, ok := avCompare(matched[i][sortKey], matched[j][sortKey])
			if !ok {
				return false
			}
			if forward {
				return cmp < 0
			}
			return cmp > 0
		})
	}
	return s.writeItemPage(w, t, req, matched, scanned)
}

// queryRangeKey is the attribute a query's results are ordered by: the
// index's sort key when one was named, otherwise the table's.
func queryRangeKey(t *dynamoTable, req map[string]any) string {
	index := str(req["IndexName"])
	if index == "" {
		_, rangeKey := keyNames(t)
		return rangeKey
	}
	for _, list := range [][]map[string]any{t.GlobalSecondaryIndexes, t.LocalSecondaryIndexes} {
		for _, ix := range list {
			if str(ix["IndexName"]) != index {
				continue
			}
			for _, el := range objList(ix["KeySchema"]) {
				if str(el["KeyType"]) == "RANGE" {
					return str(el["AttributeName"])
				}
			}
		}
	}
	return ""
}

func (s *Server) scanItems(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadDynamoTable(req)
	if aerr != nil {
		return aerr
	}
	filter, aerr := parseCondition(str(req["FilterExpression"]), exprNames(req), exprValues(req))
	if aerr != nil {
		return aerr
	}
	var matched []map[string]any
	all := s.items.all(t.TableName)
	for _, item := range all {
		if filter.matches(item) {
			matched = append(matched, item)
		}
	}
	return s.writeItemPage(w, t, req, matched, len(all))
}

// writeItemPage applies ExclusiveStartKey, Limit, Select and the projection,
// which Query and Scan share.
func (s *Server) writeItemPage(w http.ResponseWriter, t *dynamoTable, req map[string]any,
	matched []map[string]any, scanned int) *apiError {

	// Paging is by the key the client was last given, not by an offset: an
	// item inserted between two pages must not shift the ones after it.
	if start, ok := req["ExclusiveStartKey"].(map[string]any); ok && len(start) > 0 {
		from, aerr := itemKey(t, start)
		if aerr != nil {
			return aerr
		}
		for i, item := range matched {
			key, aerr := itemKey(t, item)
			if aerr != nil {
				return aerr
			}
			if key == from {
				matched = matched[i+1:]
				break
			}
		}
	}

	var last map[string]any
	if limit, ok := req["Limit"].(float64); ok && int(limit) < len(matched) {
		cut := int(limit)
		lastItem := matched[cut-1]
		matched = matched[:cut]
		// LastEvaluatedKey is only the KEY attributes, which is what the
		// client passes back as ExclusiveStartKey.
		hash, rangeKey := keyNames(t)
		last = map[string]any{}
		for _, name := range []string{hash, rangeKey} {
			if name != "" {
				last[name] = lastItem[name]
			}
		}
	}

	out := map[string]any{"Count": len(matched), "ScannedCount": scanned}
	if str(req["Select"]) == "COUNT" {
		// COUNT returns no items at all, and a client that asked for it
		// reads Count alone.
		if last != nil {
			out["LastEvaluatedKey"] = last
		}
		writeJSON(w, 200, out)
		return nil
	}

	expr := str(req["ProjectionExpression"])
	names := exprNames(req)
	items := make([]map[string]any, 0, len(matched))
	for _, item := range matched {
		projected, aerr := projection(item, expr, names)
		if aerr != nil {
			return aerr
		}
		items = append(items, projected)
	}
	out["Items"] = items
	if last != nil {
		out["LastEvaluatedKey"] = last
	}
	writeJSON(w, 200, out)
	return nil
}

// --- batches --------------------------------------------------------------

// batchWriteItem is puts and deletes across tables in one call.
//
// UnprocessedItems comes back empty, always. Real DynamoDB returns work it
// declined for throughput reasons and every SDK retries it; there is no
// throughput here to run out of, so there is never anything to hand back --
// but the field has to be PRESENT, because a client reads it before
// checking whether it is empty.
func (s *Server) batchWriteItem(w http.ResponseWriter, req map[string]any) *apiError {
	byTable, ok := req["RequestItems"].(map[string]any)
	if !ok {
		return dynamoBadRequest("RequestItems is required")
	}
	for name, raw := range byTable {
		t, aerr := s.loadDynamoTable(map[string]any{"TableName": name})
		if aerr != nil {
			return aerr
		}
		for _, write := range objList(raw) {
			if put, ok := write["PutRequest"].(map[string]any); ok {
				item, ok := put["Item"].(map[string]any)
				if !ok {
					return dynamoBadRequest("PutRequest needs an Item")
				}
				key, aerr := itemKey(t, item)
				if aerr != nil {
					return aerr
				}
				s.items.put(t.TableName, key, item)
				continue
			}
			if del, ok := write["DeleteRequest"].(map[string]any); ok {
				keyAttrs, ok := del["Key"].(map[string]any)
				if !ok {
					return dynamoBadRequest("DeleteRequest needs a Key")
				}
				key, aerr := itemKey(t, keyAttrs)
				if aerr != nil {
					return aerr
				}
				s.items.delete(t.TableName, key)
				continue
			}
			return dynamoBadRequest("a batch write must be a PutRequest or a DeleteRequest")
		}
	}
	writeJSON(w, 200, map[string]any{"UnprocessedItems": map[string]any{}})
	return nil
}

func (s *Server) batchGetItem(w http.ResponseWriter, req map[string]any) *apiError {
	byTable, ok := req["RequestItems"].(map[string]any)
	if !ok {
		return dynamoBadRequest("RequestItems is required")
	}
	responses := map[string]any{}
	for name, raw := range byTable {
		t, aerr := s.loadDynamoTable(map[string]any{"TableName": name})
		if aerr != nil {
			return aerr
		}
		spec, _ := raw.(map[string]any)
		names := map[string]string{}
		for alias, n := range mapOf(spec["ExpressionAttributeNames"]) {
			names[alias] = str(n)
		}
		expr := str(spec["ProjectionExpression"])

		items := []map[string]any{}
		for _, keyAttrs := range objList(spec["Keys"]) {
			key, aerr := itemKey(t, keyAttrs)
			if aerr != nil {
				return aerr
			}
			item := s.items.get(t.TableName, key)
			if item == nil {
				continue // a key that is not there is simply absent
			}
			projected, aerr := projection(item, expr, names)
			if aerr != nil {
				return aerr
			}
			items = append(items, projected)
		}
		responses[name] = items
	}
	writeJSON(w, 200, map[string]any{
		"Responses": responses, "UnprocessedKeys": map[string]any{},
	})
	return nil
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
