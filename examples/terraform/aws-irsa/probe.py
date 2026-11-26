# What a pod proves, in four lines of boto3 and no credentials.
#
# boto3 is given nothing: it finds AWS_ROLE_ARN and
# AWS_WEB_IDENTITY_TOKEN_FILE in the environment, reads the projected
# token off disk and calls AssumeRoleWithWebIdentity itself. So this is
# the ordinary code anyone writes against real AWS, unchanged.
import boto3
from botocore.exceptions import ClientError

print("who am I  :", boto3.client("sts").get_caller_identity()["Arn"])

s3 = boto3.client("s3")

body = s3.get_object(Bucket="nimbus-irsa-allowed", Key="hello.txt")["Body"].read()
print("allowed   :", body.decode().strip())

try:
    s3.get_object(Bucket="nimbus-irsa-denied", Key="secret.txt")
    print("denied    : *** READ SUCCEEDED -- the role should not grant this ***")
    raise SystemExit(1)
except ClientError as e:
    # The reason is in the activity feed as well. A denied GET keeps its
    # body, unlike a HEAD, which carries none by HTTP's own rule and so
    # reports only "Forbidden" with nothing about the permission.
    print("denied    :", e.response["Error"]["Code"], "-", e.response["Error"].get("Message", ""))
