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
  "aws::ec2::instance": ["EC2", "#ff9900", "EC2 instance"],
  "aws::ec2::keypair": ["KEY", "#8b949e", "Key pair"],
  "aws::s3::bucket": ["S3", "#3fb950", "S3 bucket"],
  "aws::iam::role": ["ROLE", "#bc8cff", "IAM role"],
  "aws::iam::user": ["USER", "#bc8cff", "IAM user"],
  "aws::iam::policy": ["POL", "#8b949e", "IAM policy"],
  "aws::iam::instanceprofile": ["PROF", "#8b949e", "Instance profile"],
  "aws::eks::cluster": ["EKS", "#58a6ff", "EKS cluster"],
  "aws::eks::nodegroup": ["NG", "#79c0ff", "Node group"],
  "aws::dynamodb::table": ["DDB", "#e3b341", "DynamoDB table"],
  "aws::lambda::function": ["FN", "#ff9900", "Lambda function"],
  "aws::logs::loggroup": ["LOGS", "#8b949e", "Log group"],
};

// Siblings are ordered so a stack reads the way it was built: the network
// first, then what sits in it.
export const ORDER = ["account", "vpc", "subnet", "internetgateway", "natgateway",
  "routetable", "securitygroup", "instance", "cluster", "nodegroup", "table", "function",
  "bucket", "keypair", "role", "user", "policy", "instanceprofile", "loggroup"];

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

// --- the network view's model ----------------------------------------------
//
// What the shared network view needs to know about AWS networking. The rule
// it shares with the other two clouds: a subnet is a stored resource that
// NAMES its network -- here by `vpcId`, where Azure uses a parent id and
// GCP a self link.
//
// Two differences from Azure are worth stating rather than papering over:
//
//   - **A security group is not a subnet attachment.** It belongs to the
//     VPC and attaches to an instance's interfaces, so a subnet carries
//     only its route table. Drawing a security group on a subnet would be
//     Azure's model wearing AWS's names.
//   - **A route table names the subnet, not the other way round.** The
//     association lives on the table's `associationSet`, so the chip has
//     to be found by searching the tables, which is what `chipsOf`'s second argument is
//     for -- Azure's subnet points AT its route table and needs no search.
export const NETWORK = {
  networkType: "aws::ec2::vpc",
  subnetType: "aws::ec2::subnet",
  instanceType: "aws::ec2::instance",

  prefixesOf: (r) => {
    const cidr = (r.body || {}).cidrBlock;
    return cidr ? [cidr] : [];
  },
  cidrOf: (r) => (r.body || {}).cidrBlock || "",
  networkOf: (r) => String((r.body || {}).vpcId || "").toLowerCase(),

  // A route table associated with this subnet, found by searching the
  // tables -- the association is recorded on the TABLE.
  chipsOf: (subnet, all) => {
    const key = String(subnet.id).toLowerCase();
    for (const r of all) {
      if ((r.type || "").toLowerCase() !== "aws::ec2::routetable") continue;
      const assocs = (r.body || {}).associationSet || [];
      if (assocs.some((a) => String(a.subnetId || "").toLowerCase() === key)) {
        return [{ kind: "rt", id: r.id, label: "Route table" }];
      }
    }
    return [];
  },

  // One hop: an instance names its subnet directly.
  instanceSubnet: (r) => String((r.body || {}).subnetId || "").toLowerCase(),
  instanceLabel: (r) => (r.body || {}).instanceType || "",

  // No cross-VPC relationship is modelled: peering connections round-trip
  // as state and nothing here reads them.

  // Everything EC2 that is networking and is not a VPC or a subnet: an
  // internet gateway, a NAT gateway, a security group, a route table
  // nothing is associated with.
  isNetworkResource: (type) => {
    const t = (type || "").toLowerCase();
    return t === "aws::ec2::internetgateway" || t === "aws::ec2::natgateway" ||
      t === "aws::ec2::securitygroup" || t === "aws::ec2::routetable";
  },
};

// A bucket whose own policy lets an anonymous caller in.
//
// The judgement is the SERVER's: `public` and `publicReason` are derived
// in saveBucket by asking the authorization evaluator what an anonymous
// principal may do, so this cannot disagree with what a request gets.
// Reading the policy JSON here instead would be a second implementation
// of the thing that decides, which is the mistake /api/authz exists to
// avoid -- and it would be wrong in the one case that matters, a policy
// granting `*` that a public access block neutralises.
export function exposure(r) {
  if ((r.type || "").toLowerCase() !== "aws::s3::bucket") return null;
  const body = r.body || {};
  if (!body.public) return null;
  return { label: "Public", reason: body.publicReason || "its bucket policy grants everyone" };
}
