package aws

import (
	"net/http"
	"strings"
)

// Which permission every other service's requests need.
//
// The Query services are nearly free: their action is the `Action`
// parameter with the service prefixed, so `Action=DescribeVpcs` in an
// `ec2` credential scope is `ec2:DescribeVpcs` and no table is needed.
// What a table WOULD be needed for is the resource, and for most of these
// calls there is honestly nothing to name: AWS itself requires `"*"` for
// almost every Describe, because a filter-based query has no single
// resource. So the rule here is to name the resource when the request
// names one and `*` when it does not, and to say so rather than invent a
// granularity that does not exist.
//
// The JSON services are different again. DynamoDB and CloudWatch Logs put
// the action in `X-Amz-Target` and the resource in the body; Lambda and EKS
// put both in the path. All four are tabulated below, next to the shape of
// the request rather than next to the handler, because what matters is
// that the two agree.

// --- EC2 ------------------------------------------------------------------

func (s *Server) ec2Target(r *http.Request) (target, bool) {
	p, err := readParams(r)
	if err != nil {
		return target{}, false
	}
	action := p.str("Action")
	if action == "" {
		return target{}, false
	}
	// EC2 resource-level permissions exist for a handful of actions and
	// are "*" for the rest, including every Describe. Nothing here is
	// narrowed, which can only ever grant more than real AWS would -- the
	// direction a client cannot be hurt by, and the same choice as
	// ignoring an unknown describe filter.
	return target{Action: "ec2:" + action, Resource: "*"}, true
}

// --- IAM ------------------------------------------------------------------

// iamResourceParams are the parameters that name the entity an IAM call is
// about, and the ARN kind each implies. IAM's resource ARNs are
// `arn:aws:iam::acct:role/name`, which is also the store's id -- see
// iamARN -- so this needs no lookup.
var iamResourceParams = []struct{ param, kind string }{
	{"RoleName", "role"},
	{"UserName", "user"},
	{"InstanceProfileName", "instance-profile"},
}

func (s *Server) iamTarget(r *http.Request) (target, bool) {
	p, err := readParams(r)
	if err != nil {
		return target{}, false
	}
	action := p.str("Action")
	if action == "" {
		return target{}, false
	}

	resource := "*"
	// A PolicyArn is already an ARN; everything else is a name that has
	// to be turned into one.
	if arn := p.str("PolicyArn"); arn != "" {
		resource = arn
	} else {
		for _, candidate := range iamResourceParams {
			if name := p.str(candidate.param); name != "" {
				resource = s.iamARN(candidate.kind, p.strOr("Path", "/"), name)
				break
			}
		}
	}
	return target{Action: "iam:" + action, Resource: resource}, true
}

// --- the JSON services, whose action is a header --------------------------

// jsonTarget covers DynamoDB and CloudWatch Logs: `X-Amz-Target:
// DynamoDB_20120810.PutItem`, the body carrying whatever the call is about.
//
// The two spell their fields differently -- Logs is lowerCamelCase
// (`logGroupName`) where DynamoDB is TitleCase (`TableName`) -- which is
// why the body key is passed in rather than guessed. "It is JSON like the
// other one" is not enough to go on, as logs.go already says.
func (s *Server) jsonTarget(r *http.Request, service, bodyKey string, arn func(string) string) (target, bool) {
	action := r.Header.Get("X-Amz-Target")
	if i := strings.LastIndex(action, "."); i >= 0 {
		action = action[i+1:]
	}
	if action == "" {
		return target{}, false
	}

	resource := "*"
	var body map[string]any
	if readJSON(r, &body) == nil {
		if name := str(body[bodyKey]); name != "" {
			resource = arn(name)
		}
		// Tagging and untagging name the thing by ARN instead.
		if named := str(body["ResourceArn"]); named != "" {
			resource = named
		}
		if named := str(body["resourceArn"]); named != "" {
			resource = named
		}
	}
	return target{Action: service + ":" + action, Resource: resource}, true
}

func (s *Server) dynamoTarget(r *http.Request) (target, bool) {
	return s.jsonTarget(r, "dynamodb", "TableName", s.dynamoTableARN)
}

func (s *Server) logsTarget(r *http.Request) (target, bool) {
	return s.jsonTarget(r, "logs", "logGroupName", s.logGroupARN)
}

// --- Lambda ---------------------------------------------------------------

// lambdaTarget mirrors serveLambda's switch. Lambda's paths are versioned
// per OPERATION rather than per service, so the version segment is ignored
// and the shape after it is what matters.
func (s *Server) lambdaTarget(r *http.Request) (target, bool) {
	parts := pathParts(r.URL.Path)
	all := target{Resource: "*"}

	// /{version}/tags/{arn}, with the ARN URL-encoded into ONE segment,
	// colons and all.
	if len(parts) == 3 && parts[1] == "tags" {
		arn := lambdaName(parts[2])
		switch r.Method {
		case http.MethodGet:
			return target{Action: "lambda:ListTags", Resource: parts[2]}, true
		case http.MethodPost:
			return target{Action: "lambda:TagResource", Resource: arn}, true
		case http.MethodDelete:
			return target{Action: "lambda:UntagResource", Resource: arn}, true
		}
		return target{}, false
	}

	if len(parts) < 2 || parts[1] != "functions" {
		return target{}, false
	}
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodPost:
			return target{Action: "lambda:CreateFunction", Resource: all.Resource}, true
		case http.MethodGet:
			return target{Action: "lambda:ListFunctions", Resource: all.Resource}, true
		}
		return target{}, false
	}

	arn := s.lambdaARN(lambdaName(parts[2]))
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			return target{Action: "lambda:GetFunction", Resource: arn}, true
		case http.MethodDelete:
			return target{Action: "lambda:DeleteFunction", Resource: arn}, true
		}
		return target{}, false
	}

	switch parts[3] {
	case "configuration":
		switch r.Method {
		case http.MethodGet:
			return target{Action: "lambda:GetFunctionConfiguration", Resource: arn}, true
		case http.MethodPut:
			return target{Action: "lambda:UpdateFunctionConfiguration", Resource: arn}, true
		}
	case "code":
		if r.Method == http.MethodPut {
			return target{Action: "lambda:UpdateFunctionCode", Resource: arn}, true
		}
	case "invocations":
		return target{Action: "lambda:InvokeFunction", Resource: arn}, true
	case "versions":
		if r.Method == http.MethodGet {
			return target{Action: "lambda:ListVersionsByFunction", Resource: arn}, true
		}
	case "policy":
		switch r.Method {
		case http.MethodGet:
			return target{Action: "lambda:GetPolicy", Resource: arn}, true
		case http.MethodPost:
			return target{Action: "lambda:AddPermission", Resource: arn}, true
		case http.MethodDelete:
			return target{Action: "lambda:RemovePermission", Resource: arn}, true
		}
	case "code-signing-config":
		if r.Method == http.MethodGet {
			return target{Action: "lambda:GetFunctionCodeSigningConfig", Resource: arn}, true
		}
	}
	return target{}, false
}

// --- EKS ------------------------------------------------------------------

// eksTarget mirrors serveEKS's switch.
func (s *Server) eksTarget(r *http.Request) (target, bool) {
	parts := pathParts(r.URL.Path)
	if len(parts) == 0 || parts[0] != "clusters" {
		return target{}, false
	}

	switch len(parts) {
	case 1:
		switch r.Method {
		case http.MethodPost:
			return target{Action: "eks:CreateCluster", Resource: "*"}, true
		case http.MethodGet:
			return target{Action: "eks:ListClusters", Resource: "*"}, true
		}
	case 2:
		arn := s.eksClusterARN(parts[1])
		switch r.Method {
		case http.MethodGet:
			return target{Action: "eks:DescribeCluster", Resource: arn}, true
		case http.MethodDelete:
			return target{Action: "eks:DeleteCluster", Resource: arn}, true
		}
	case 3:
		if parts[2] == "node-groups" {
			arn := s.eksClusterARN(parts[1])
			switch r.Method {
			case http.MethodPost:
				return target{Action: "eks:CreateNodegroup", Resource: arn}, true
			case http.MethodGet:
				return target{Action: "eks:ListNodegroups", Resource: arn}, true
			}
		}
	case 4:
		if parts[2] == "node-groups" {
			arn := s.eksNodegroupARN(parts[1], parts[3])
			switch r.Method {
			case http.MethodGet:
				return target{Action: "eks:DescribeNodegroup", Resource: arn}, true
			case http.MethodDelete:
				return target{Action: "eks:DeleteNodegroup", Resource: arn}, true
			}
		}
	}
	return target{}, false
}
