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
    # A STREAM event, delivered by the event source mapping. It carries
    # Records rather than an id, and the handler must NOT write back:
    # writing to the table that triggers you is an infinite loop, in real
    # AWS exactly as here. Measured before this guard existed -- the table
    # went from 14 rows to 19 in four seconds and kept going.
    #
    # Real AWS gives you no protection from this. The usual answers are a
    # second table, a marker attribute the handler skips, or a filter
    # criterion on the mapping; recognising the event shape is the
    # smallest one that fits an example.
    if "Records" in event:
        for record in event["Records"]:
            keys = record.get("dynamodb", {}).get("Keys", {})
            print(f"stream saw {record['eventName']} on {keys}")
        return {"sawRecords": len(event["Records"])}

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
