// AWS's resource types.
//
// The keys are lower-cased type strings, the same shape Azure uses, but AWS
// has no type names of its own on the wire -- a VPC is just a VPC -- so
// Nimbus gives them CloudFormation-style names (`AWS::EC2::VPC`), which are
// the closest thing AWS has to a canonical type and what anyone who has
// read a template will recognise.

export const TYPES = {
  "aws::account": ["ACCT", "#ff9900", "Account"],
  "aws::ec2::vpc": ["VPC", "#39c5cf", "Virtual private cloud"],
  "aws::ec2::subnet": ["SNET", "#56d4bc", "Subnet"],
  "aws::ec2::internetgateway": ["IGW", "#39c5cf", "Internet gateway"],
  "aws::ec2::natgateway": ["NAT", "#39c5cf", "NAT gateway"],
  "aws::ec2::routetable": ["RT", "#a5d6ff", "Route table"],
  "aws::ec2::route": ["ROUTE", "#a5d6ff", "Route"],
  "aws::ec2::securitygroup": ["SG", "#f0883e", "Security group"],
  "aws::ec2::networkacl": ["NACL", "#f0883e", "Network ACL"],
  "aws::ec2::eip": ["EIP", "#d2a8ff", "Elastic IP"],
  "aws::s3::bucket": ["S3", "#3fb950", "S3 bucket"],
  "aws::iam::role": ["ROLE", "#bc8cff", "IAM role"],
  "aws::iam::user": ["USER", "#bc8cff", "IAM user"],
  "aws::iam::policy": ["POL", "#8b949e", "IAM policy"],
  "aws::iam::instanceprofile": ["PROF", "#8b949e", "Instance profile"],
  "aws::eks::cluster": ["EKS", "#58a6ff", "EKS cluster"],
  "aws::eks::nodegroup": ["NG", "#79c0ff", "Node group"],
  "aws::dynamodb::table": ["DDB", "#e3b341", "DynamoDB table"],
};

// Siblings are ordered so a stack reads the way it was built: the network
// first, then what sits in it.
export const ORDER = ["account", "vpc", "subnet", "internetgateway", "natgateway",
  "routetable", "securitygroup", "cluster", "nodegroup", "table", "bucket", "role", "user", "policy"];

// AWS has no addressing-only types to hide, unlike ARM's blobServices.
export const PASSTHROUGH = null;

// The type at the top of the tree, which the toolbar's toggle hides.
export const ROOT_TYPE = "aws::account";
export const ROOT_LABEL = "account";

// AWS resources are not grouped the way an Azure resource group groups
// them, so the plan overlay has no container to park a pending create in.
export const GROUP_TYPE = null;

// The type whose children the Kubernetes view draws. EKS clusters are real
// k3s clusters, mirrored by the same engine AKS uses, so that view works
// here with no code of its own -- it only has to know what a cluster is
// called in this cloud.
export const K8S_CLUSTER_TYPE = "aws::eks::cluster";

export function displayName(r) {
  return r.name;
}
