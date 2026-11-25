package aws

import (
	"context"
	"math/big"
	"time"
)

// Time to live: items that delete themselves.
//
// `UpdateTimeToLive` round-tripped as a setting and nothing ever expired,
// which is the shape of divergence this project keeps finding -- a
// configuration that applies cleanly and does nothing.
//
// # It is a BACKGROUND deletion, and that is the whole design
//
// DynamoDB does not filter expired items out of reads. A sweeper deletes
// them, and until it does an expired item is still there and still
// returned. That matters more than it sounds: TTL deletions appear in the
// table's STREAM as ordinary REMOVE records, which is how the pattern
// people actually use TTL for works -- an item expires, the stream
// delivers the removal, and a Lambda archives it. Filtering on read
// instead would make the item vanish with no record of it going.
//
// So Nimbus sweeps rather than filtering, and reads are untouched.
//
// # The divergence, stated
//
// Real DynamoDB expires items within 48 hours of the timestamp, not
// promptly -- the documentation is explicit that it is best-effort.
// Nimbus sweeps every ttlSweepInterval, which is far prompter. Prompter
// is the useful direction for a local emulator and the one a test can
// rely on, and it is the direction that cannot hide a bug: anything
// correct against real DynamoDB's 48-hour window is correct against a
// one-second one.

// ttlSweepInterval is how often expired items are collected.
const ttlSweepInterval = time.Second

// startTTLSweeper runs the sweep until the context is cancelled.
func (s *Server) startTTLSweeper(ctx context.Context) {
	ticker := time.NewTicker(ttlSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepExpired(time.Now())
		}
	}
}

// sweepExpired deletes every item whose TTL attribute has passed, in every
// table with TTL enabled.
func (s *Server) sweepExpired(now time.Time) int {
	removed := 0
	for _, t := range s.allDynamoTables() {
		attr := ttlAttributeOf(t)
		if attr == "" {
			continue
		}
		for _, item := range s.items.all(t.TableName) {
			if !itemHasExpired(item, attr, now) {
				continue
			}
			key, aerr := itemKey(t, item)
			if aerr != nil {
				continue
			}
			if old := s.items.delete(t.TableName, key); old != nil {
				// A TTL deletion is an ordinary REMOVE in the stream, and
				// that is the point of it: the pattern people use TTL for
				// is to archive what expired, which needs the removal to
				// be delivered.
				s.recordTTLDeletion(t, old)
				removed++
			}
		}
	}
	return removed
}

// ttlAttributeOf is the attribute TTL is read from, or "" when the table
// has TTL disabled or never configured it.
func ttlAttributeOf(t *dynamoTable) string {
	if t.NimbusTTL == nil || str(t.NimbusTTL["TimeToLiveStatus"]) != "ENABLED" {
		return ""
	}
	return str(t.NimbusTTL["AttributeName"])
}

// itemHasExpired reads the TTL attribute and compares it to now.
//
// The attribute must be a NUMBER holding a unix time in SECONDS. Anything
// else -- a string, a missing attribute, a number that will not parse --
// means the item simply does not expire, which is what real DynamoDB
// does: it is explicit that items with a malformed or absent TTL
// attribute are left alone. Guessing at a date in a string would delete
// data on a misreading, and deleting too much is the one mistake here
// that cannot be undone.
func itemHasExpired(item map[string]any, attr string, now time.Time) bool {
	value, ok := item[attr].(map[string]any)
	if !ok {
		return false
	}
	raw, isNumber := value["N"].(string)
	if !isNumber {
		return false
	}
	// A DynamoDB number is a decimal STRING because it does not fit a
	// float -- up to 38 significant digits -- so it goes through
	// math/big like every other number here rather than ParseFloat.
	seconds, ok := new(big.Float).SetString(raw)
	if !ok {
		return false
	}
	epoch, _ := seconds.Int64()
	return epoch <= now.Unix()
}

// recordTTLDeletion writes the stream record for an expired item.
//
// It carries the `userIdentity` real DynamoDB stamps on a TTL deletion --
// the service principal -- which is the ONLY thing distinguishing an
// expiry from a client's own DeleteItem in the stream. A consumer that
// archives expired items reads exactly this field to tell them apart, so
// leaving it out would make the two indistinguishable.
//
// The field names are CAPITALISED here because that is the DynamoDB
// Streams API's own spelling, and botocore drops a member it does not
// recognise: with lowercase keys `aws dynamodbstreams get-records`
// reported `"userIdentity": {}` -- present, empty, and saying nothing.
// Measured. The LAMBDA event spells the same thing in lowerCamelCase,
// because that payload is built by the Lambda service rather than
// returned by this API; streamEventRecords translates it.
func (s *Server) recordTTLDeletion(t *dynamoTable, old map[string]any) {
	s.recordChangeAs(t, nil, old, map[string]any{
		"PrincipalId": "dynamodb.amazonaws.com",
		"Type":        "Service",
	})
}

// lambdaUserIdentity is the same identity in the spelling a Lambda event
// uses. Two spellings for one concept, which is not a style choice: a
// handler reads `record["userIdentity"]["principalId"]` and botocore
// reads `PrincipalId`, and each finds nothing in the other's.
func lambdaUserIdentity(identity map[string]any) map[string]any {
	if identity == nil {
		return nil
	}
	return map[string]any{
		"principalId": identity["PrincipalId"],
		"type":        identity["Type"],
	}
}
