package aws

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// The instance metadata service, as far as a container can reach it.
//
// Real IMDS is 169.254.169.254, a link-local address the instance's own
// kernel routes. A Docker container cannot reach that without adding a
// route on the host, which needs root -- so Nimbus serves each instance's
// metadata on its own ordinary port and tells the container where it is
// with AWS_EC2_METADATA_SERVICE_ENDPOINT, which every current SDK reads.
//
// What that buys: a program inside an instance can ask which instance it
// is, and `aws ec2 describe-instances` from inside works through the
// endpoint it was given. What it does NOT: code written against the
// literal 169.254.169.254 still fails, and so does anything reading
// credentials from the IAM role path, because Nimbus mints none. Both are
// stated rather than half-emulated.
//
// The identity problem is solved by the PORT, not by the request. One
// listener per instance means there is nothing to identify: whoever
// connects to this port is that instance. Mapping a shared listener's
// requests back to an instance by source address would not work anyway --
// a container reaching the host through the bridge gateway is seen as the
// gateway, not as itself.

// imdsToken is what IMDSv2's PUT /latest/api/token answers. The SDKs send
// it back in X-aws-ec2-metadata-token on every call; nothing verifies it,
// because a token this service issued to the only client that can reach
// this port proves nothing worth checking.
const imdsToken = "nimbus-imds-token"

// instanceMetadata builds the handler for one instance's metadata.
func (s *Server) instanceMetadata(instanceID string) http.Handler {
	mux := http.NewServeMux()

	// IMDSv2: a session token first. Answering this is what stops a modern
	// SDK falling back and then giving up.
	mux.HandleFunc("/latest/api/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, imdsToken)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/latest/")
		i, err := load(s, instanceID, &instance{})
		if err != nil || i.InstanceID == "" {
			http.NotFound(w, r)
			return
		}
		value, ok := s.metadataValue(i, path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, value)
	})
	return mux
}

// metadataValue answers one metadata path. The listing form -- a path
// ending in "/" -- returns the keys below it, one per line, which is how a
// client discovers what is there.
func (s *Server) metadataValue(i *instance, path string) (string, bool) {
	doc := map[string]string{
		"meta-data/ami-id":                      i.ImageID,
		"meta-data/instance-id":                 i.InstanceID,
		"meta-data/instance-type":               i.InstanceType,
		"meta-data/local-hostname":              i.PrivateDNS,
		"meta-data/local-ipv4":                  i.PrivateIP,
		"meta-data/hostname":                    i.PrivateDNS,
		"meta-data/placement/availability-zone": i.Placement.AvailabilityZone,
		"meta-data/placement/region":            s.cfg.Region,
		"meta-data/mac":                         macFor(i.InstanceID),
		"meta-data/security-groups":             groupNames(i.Groups),
		"meta-data/reservation-id":              i.ReservationID,
		"meta-data/profile":                     "default-hvm",
		"meta-data/services/domain":             "amazonaws.com",
		"meta-data/services/partition":          "aws",
		"dynamic/instance-identity/document":    identityDocument(i, s.cfg.Region, s.cfg.AccountID),
		"user-data":                             i.UserData,
	}
	if v, ok := doc[path]; ok {
		return v, true
	}

	// A listing: every key one level below this prefix.
	if path == "" || strings.HasSuffix(path, "/") {
		var keys []string
		seen := map[string]bool{}
		for k := range doc {
			if !strings.HasPrefix(k, path) {
				continue
			}
			rest := strings.TrimPrefix(k, path)
			if cut := strings.Index(rest, "/"); cut >= 0 {
				rest = rest[:cut+1]
			}
			if rest != "" && !seen[rest] {
				seen[rest] = true
				keys = append(keys, rest)
			}
		}
		if len(keys) > 0 {
			return strings.Join(sorted(keys), "\n"), true
		}
	}
	return "", false
}

// identityDocument is the JSON blob SDKs read to learn the region and
// account without calling STS.
func identityDocument(i *instance, region, account string) string {
	b, err := json.Marshal(map[string]any{
		"accountId":               account,
		"architecture":            i.Architecture,
		"availabilityZone":        i.Placement.AvailabilityZone,
		"imageId":                 i.ImageID,
		"instanceId":              i.InstanceID,
		"instanceType":            i.InstanceType,
		"privateIp":               i.PrivateIP,
		"region":                  region,
		"version":                 "2017-09-30",
		"pendingTime":             i.LaunchTime,
		"marketplaceProductCodes": nil,
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func groupNames(groups []groupRef) string {
	var out []string
	for _, g := range groups {
		if g.GroupName != "" {
			out = append(out, g.GroupName)
		} else {
			out = append(out, g.GroupID)
		}
	}
	return strings.Join(out, "\n")
}

// macFor is derived from the instance id, so it is the same on every read.
// The 02 first octet marks it locally administered, which is what it is.
func macFor(instanceID string) string {
	h := stableID(instanceID, 10)
	return "02:" + h[0:2] + ":" + h[2:4] + ":" + h[4:6] + ":" + h[6:8] + ":" + h[8:10]
}

func sorted(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
