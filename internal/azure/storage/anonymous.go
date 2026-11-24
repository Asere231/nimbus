package storage

import (
	"net/http"
	"strconv"
)

// Anonymous access, which a container's public access level decides.
//
// This was a real hole rather than a missing feature. PublicAccess has been
// stored, persisted, returned in the container listing and echoed back as
// x-ms-blob-public-access since the blob service was built -- and never read
// in a decision, so every unauthenticated request to a PRIVATE container was
// served. It is the Azure twin of the AWS anonymous-principal bug, where an
// unsigned request resolved to the account root and was therefore unrestricted.
//
// It belongs with the SAS work because omitting the SAS entirely was otherwise
// the easiest way past it: enforcing what a token grants means nothing while
// sending no token at all grants everything.
//
// Two levels, and the difference between them is the whole reason there are
// two: "blob" lets anyone who knows a blob's name read it, and "container"
// additionally lets them LIST, which is how they would learn the names.

// authorizeAnonymous decides a request that arrived with no credentials.
func authorizeAnonymous(r *http.Request, a *Account, svc Service) *accessRefusal {
	// Only blob has anonymous access in Azure at all. Queue, table and file
	// always require a credential, so there is no public level to consult.
	if svc != ServiceBlob {
		return refuse("AuthenticationFailed",
			"The "+serviceLabel(svc)+" service requires a credential; "+
				"anonymous access exists only for blobs, and only when the container allows it.")
	}

	container, blob := splitBlobPath(r.URL.Path)

	// The operation is classified by the SAME function a SAS uses. Reusing it
	// is not a convenience: two classifiers over one router would be two
	// things to keep in step with it, and the one that drifted would decide
	// differently from the one that was tested.
	op, known := blobSASOperation(r, container, blob)
	if !known {
		return refuse("AuthenticationFailed",
			"Nimbus cannot classify this operation, so it is refused for an anonymous caller: "+
				r.Method+" "+strconv.Quote(r.URL.Path)+".")
	}

	// Anonymous is read-only whatever the access level says. Real Azure has no
	// public level that permits a write, and inventing one would teach the
	// opposite of the truth.
	if op.perms != "r" && op.perms != "l" {
		return refuse("AuthenticationFailed",
			"Anonymous access is read-only and "+op.name+" is a write. "+
				"Use the account key, or a SAS granting "+sasPermsPhrase(op.perms)+".")
	}

	// Account-level operations are never anonymous: there is no container to
	// carry a public access level, so nothing could ever permit them.
	if op.level == 's' {
		return refuse("AuthenticationFailed",
			op.name+" is an account-level operation and is never anonymous.")
	}

	c := a.Container(container)
	if c == nil {
		// A container that is not there and one an anonymous caller may not
		// reach answer the same way, which is deliberate -- see below.
		return anonymousNotFound()
	}

	switch c.PublicAccess {
	case "container":
		// Full public read: the blobs and the list of them.
		return nil
	case "blob":
		// The blobs but NOT the list. Someone who knows a name may read it;
		// learning the names is what "container" additionally allows.
		if op.level == 'o' {
			return nil
		}
		return refuse("AuthenticationFailed",
			"Container "+strconv.Quote(container)+" allows anonymous access to its BLOBS "+
				"(x-ms-blob-public-access: blob) but not to "+op.name+
				". That needs public access level \"container\".")
	default:
		return anonymousNotFound()
	}
}

// anonymousNotFound is 404, not 403, and the difference is the point.
//
// Azure does not tell an unauthenticated caller that a container exists but is
// private, because that is itself information: a 403 would let anyone
// enumerate container names by watching which refusals differ. So a private
// container and an absent one answer identically.
//
// It takes no arguments on purpose. Every other refusal in this package names
// what it was about, which is the whole reason they are readable -- and this
// is the one where doing so would hand over exactly what the 404 exists to
// withhold. Worth a sentence, because "add the container name like the others"
// looks like an improvement.
func anonymousNotFound() *accessRefusal {
	return &accessRefusal{
		Code:   "ResourceNotFound",
		Msg:    "The specified resource does not exist.",
		Status: http.StatusNotFound,
	}
}
