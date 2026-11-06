"""A handler written exactly as it would be for real AWS.

There is no endpoint anywhere in this file. boto3 reads AWS_ENDPOINT_URL,
which Nimbus sets on the container, so the same code runs here and in
production -- which is the whole point of running it in AWS's own base
image rather than interpreting it.
"""

import json
import os
import time

import boto3

ddb = boto3.client("dynamodb")
TABLE = os.environ["TABLE"]


def handler(event, context):
    item_id = str(event.get("id", int(time.time())))
    ddb.put_item(
        TableName=TABLE,
        Item={
            "id": {"S": item_id},
            "note": {"S": event.get("note", "written by the writer function")},
            "at": {"N": str(int(time.time()))},
        },
    )
    # Anything printed lands in /aws/lambda/<name>, so
    # `aws logs filter-log-events` shows it afterwards.
    print(f"wrote {item_id} to {TABLE}")
    return {"written": item_id, "table": TABLE}
