package aws

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
)

// AWS's own managed policies.
//
// A role in a real configuration is usually granted through one of these
// rather than through a document somebody wrote, so with none of them a
// first enforced apply would fail on scaffolding rather than on the policy
// under test. Copying all of them is not an option either: there are
// hundreds, several are thousands of lines, and a partial transcription
// that silently grants too much is worse than no transcription at all.
//
// So the selection rule is narrow and stated: **a policy is bundled only if
// its real document is short enough to transcribe exactly.** Everything
// else is absent, and an absent one REFUSES, naming itself -- never
// "grants nothing" (which looks like a policy that does not cover the
// action) and never "grants everything". Same rule as a DynamoDB
// FilterExpression this emulator cannot parse.
//
// These are transcribed from AWS's published documents and are not fetched
// or verified at runtime, so they can drift as AWS revises a version.
// `docs/ROADMAP.md` says so under "Later".
var awsManagedPolicies = map[string]string{
	"arn:aws:iam::aws:policy/AdministratorAccess": `{
	  "Version": "2012-10-17",
	  "Statement": [{"Effect": "Allow", "Action": "*", "Resource": "*"}]
	}`,

	"arn:aws:iam::aws:policy/AmazonS3FullAccess": `{
	  "Version": "2012-10-17",
	  "Statement": [{
	    "Effect": "Allow",
	    "Action": ["s3:*", "s3-object-lambda:*"],
	    "Resource": "*"
	  }]
	}`,

	"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess": `{
	  "Version": "2012-10-17",
	  "Statement": [{
	    "Effect": "Allow",
	    "Action": ["s3:Get*", "s3:List*", "s3:Describe*",
	               "s3-object-lambda:Get*", "s3-object-lambda:List*"],
	    "Resource": "*"
	  }]
	}`,

	"arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole": `{
	  "Version": "2012-10-17",
	  "Statement": [{
	    "Effect": "Allow",
	    "Action": ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"],
	    "Resource": "*"
	  }]
	}`,

	"arn:aws:iam::aws:policy/AWSLambdaExecute": `{
	  "Version": "2012-10-17",
	  "Statement": [
	    {"Effect": "Allow", "Action": ["logs:*"], "Resource": "arn:aws:logs:*:*:*"},
	    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject"],
	     "Resource": "arn:aws:s3:::*/*"}
	  ]
	}`,

	"arn:aws:iam::aws:policy/AmazonEC2ReadOnlyAccess": `{
	  "Version": "2012-10-17",
	  "Statement": [
	    {"Effect": "Allow", "Action": "ec2:Describe*", "Resource": "*"},
	    {"Effect": "Allow", "Action": "elasticloadbalancing:Describe*", "Resource": "*"},
	    {"Effect": "Allow",
	     "Action": ["cloudwatch:ListMetrics", "cloudwatch:GetMetricStatistics",
	                "cloudwatch:Describe*"],
	     "Resource": "*"},
	    {"Effect": "Allow", "Action": "autoscaling:Describe*", "Resource": "*"}
	  ]
	}`,
}

// managedPolicyDocument finds a bundled document.
//
// The lookup tolerates the path a client attached it with: the same policy
// is written both `.../AWSLambdaBasicExecutionRole` and
// `.../service-role/AWSLambdaBasicExecutionRole`, and a configuration that
// uses the shorter spelling means the same policy.
func managedPolicyDocument(arn string) (string, error) {
	if doc, ok := awsManagedPolicies[arn]; ok {
		return doc, nil
	}
	name := arn[strings.LastIndex(arn, "/")+1:]
	for known, doc := range awsManagedPolicies {
		if known[strings.LastIndex(known, "/")+1:] == name {
			return doc, nil
		}
	}
	return "", fmt.Errorf("Nimbus does not bundle the AWS managed policy %s, "+
		"so it cannot tell what it grants; attach a policy of your own instead "+
		"(bundled: %s)", arn, strings.Join(bundledPolicyNames(), ", "))
}

// managedPolicyRules compiles one managed policy. An unknown one still
// produces a rule -- one that refuses and explains itself -- so that
// evaluation answers AccessDenied with a reason rather than failing as a
// server error.
func managedPolicyRules(arn string) ([]authz.Rule, error) {
	doc, err := managedPolicyDocument(arn)
	if err != nil {
		return []authz.Rule{&brokenRule{source: "managed policy " + arn, err: err}}, err
	}
	return compileDocument("managed policy "+arn, doc)
}

func bundledPolicyNames() []string {
	out := make([]string, 0, len(awsManagedPolicies))
	for arn := range awsManagedPolicies {
		out = append(out, arn[strings.LastIndex(arn, "/")+1:])
	}
	sort.Strings(out)
	return out
}
