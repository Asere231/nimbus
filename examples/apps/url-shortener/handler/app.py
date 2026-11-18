"""A link shortener, written the way one would be written for real AWS.

There is no endpoint in this file. Every client is constructed the way the
boto3 documentation says to construct it, and it reaches Nimbus because
AWS_ENDPOINT_URL is in the environment -- which is the whole point: a
handler written against AWS runs here unchanged.

Four actions, each one a thing a shortener actually has to do:

  create   store a code -> url mapping, REFUSING to clobber one that exists
  resolve  read it back and count the hit
  list     every link one owner made, newest first
  publish  write a static index page to S3

What they exercise between them is a conditional write, an atomic counter,
a query against a secondary index with a descending sort, and an object
upload -- rather than the single PutItem a hello-world would.
"""

import datetime
import hashlib
import json
import os

import boto3

TABLE = os.environ["TABLE"]
BUCKET = os.environ["BUCKET"]
BASE = os.environ.get("BASE_URL", "https://nimb.us")

dynamodb = boto3.client("dynamodb")
s3 = boto3.client("s3")


def code_for(url):
    """A short code derived from the url, so the same url shortens the same
    way twice. Six base-36-ish characters off a sha256."""
    digest = hashlib.sha256(url.encode()).hexdigest()
    return digest[:7]


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def create(event):
    url = event["url"]
    owner = event.get("owner", "anonymous")
    code = event.get("code") or code_for(url)

    try:
        dynamodb.put_item(
            TableName=TABLE,
            Item={
                "code": {"S": code},
                "url": {"S": url},
                "owner": {"S": owner},
                "created": {"S": now()},
                "hits": {"N": "0"},
            },
            # The point of a shortener: a code is claimed once. Without this
            # a second create silently steals someone else's link.
            ConditionExpression="attribute_not_exists(code)",
        )
    except dynamodb.exceptions.ConditionalCheckFailedException:
        existing = dynamodb.get_item(TableName=TABLE, Key={"code": {"S": code}})
        item = existing.get("Item", {})
        if item.get("url", {}).get("S") == url:
            # Same url, same code: idempotent, which is the right answer.
            return {"code": code, "short": f"{BASE}/{code}", "created": False}
        return {"error": "code already taken", "code": code}

    print(f"created {code} -> {url} for {owner}")
    return {"code": code, "short": f"{BASE}/{code}", "created": True}


def resolve(event):
    code = event["code"]
    # One call that both reads and counts: an atomic ADD, so two concurrent
    # resolves cannot lose a hit the way a get-then-put would.
    out = dynamodb.update_item(
        TableName=TABLE,
        Key={"code": {"S": code}},
        UpdateExpression="ADD hits :one",
        ExpressionAttributeValues={":one": {"N": "1"}},
        ConditionExpression="attribute_exists(code)",
        ReturnValues="ALL_NEW",
    )
    item = out["Attributes"]
    print(f"resolved {code} (hit {item['hits']['N']})")
    return {
        "code": code,
        "url": item["url"]["S"],
        "hits": int(item["hits"]["N"]),
    }


def list_links(event):
    owner = event.get("owner", "anonymous")
    out = dynamodb.query(
        TableName=TABLE,
        IndexName="by-owner",
        # "owner" is a DynamoDB RESERVED WORD, so it cannot appear in an
        # expression directly -- real DynamoDB answers
        # ValidationException. Hence the #o placeholder, which is what the
        # documentation tells you to do and what any real deployment ends up
        # with. (Nimbus does not police the reserved list, so the plain
        # spelling works here and would fail in production.)
        KeyConditionExpression="#o = :o",
        ExpressionAttributeNames={"#o": "owner"},
        ExpressionAttributeValues={":o": {"S": owner}},
        ScanIndexForward=False,  # newest first
    )
    return {
        "owner": owner,
        "count": out["Count"],
        "links": [
            {
                "code": i["code"]["S"],
                "url": i["url"]["S"],
                "hits": int(i.get("hits", {"N": "0"})["N"]),
            }
            for i in out["Items"]
        ],
    }


def publish(event):
    owner = event.get("owner", "anonymous")
    links = list_links({"owner": owner})["links"]
    rows = "\n".join(
        f'    <li><a href="{BASE}/{l["code"]}">{l["code"]}</a> '
        f'&rarr; {l["url"]} <small>({l["hits"]} hits)</small></li>'
        for l in links
    )
    page = (
        "<!doctype html>\n<html><head><meta charset=utf-8>"
        f"<title>Links by {owner}</title></head>\n<body>\n"
        f"  <h1>Links by {owner}</h1>\n  <ul>\n{rows}\n  </ul>\n"
        f"  <p>Generated {now()}</p>\n</body></html>\n"
    )
    key = f"pages/{owner}/index.html"
    s3.put_object(
        Bucket=BUCKET,
        Key=key,
        Body=page.encode(),
        ContentType="text/html",
        Metadata={"owner": owner, "links": str(len(links))},
    )
    print(f"published {key} ({len(page)} bytes, {len(links)} links)")
    return {"bucket": BUCKET, "key": key, "bytes": len(page), "links": len(links)}


ACTIONS = {
    "create": create,
    "resolve": resolve,
    "list": list_links,
    "publish": publish,
}


def handler(event, context):
    action = event.get("action", "create")
    fn = ACTIONS.get(action)
    if fn is None:
        raise ValueError(f"unknown action {action!r}; try {sorted(ACTIONS)}")
    return fn(event)
