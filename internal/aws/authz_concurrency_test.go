package aws

import (
	"net/http"
	"sync"
	"testing"
)

// Two sub-resource writes to the same bucket, at once.
//
// The aws provider applies independent resources CONCURRENTLY, so an
// apply carrying both aws_s3_bucket_policy and
// aws_s3_bucket_public_access_block really does send these together.
// Load-mutate-save re-read the bucket each time and still lost one:
// both handlers read the same copy and the second to write restored the
// first one's fields.
//
// Found by a CI assertion, not by reasoning -- a step that set a policy
// and a block in one apply then read the object back and found the
// block gone. Without updateBucket this fails on nearly every one of 20
// runs; it is the lost update core.Store.Update exists for, and it had
// been here since bucket sub-resources were built.
func TestConcurrentBucketConfigWritesDoNotLoseEachOther(t *testing.T) {
	s := newTestServer(t)
	seedBucket(t, s, "open")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		asKey(t, s, "nimbus", "s3", http.MethodPut, "/open?policy",
			`{"Version":"2012-10-17","Statement":[{"Sid":"X","Effect":"Allow",`+
				`"Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)
	}()
	go func() {
		defer wg.Done()
		asKey(t, s, "nimbus", "s3", http.MethodPut, "/open?publicAccessBlock",
			`<PublicAccessBlockConfiguration><RestrictPublicBuckets>true`+
				`</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	}()
	wg.Wait()

	b := bucketBody(t, s, "open")
	if b.Policy == "" {
		t.Error("the bucket policy was lost by a concurrent write")
	}
	if b.PublicAccess == nil || !b.PublicAccess.RestrictPublicBuckets {
		t.Error("the public access block was lost by a concurrent write")
	}
}
