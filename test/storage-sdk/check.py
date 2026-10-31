#!/usr/bin/env python3
"""Drive Nimbus's storage data plane with the real Azure SDKs for Python.

This is the counterpart to test/sdkcompat, which covers the ARM control plane
with go-azure-sdk. Here the point is a *different client stack*: these are the
same libraries the `az` CLI is built on, so they exercise the XML and OData
wire formats, Shared Key signing, and the blob/queue/table protocols the way a
real application would.

Usage:

    ./bin/nimbus start --persist          # terminal 1
    cd test/storage-sdk && ./run.sh       # terminal 2

run.sh creates the venv and passes the account through. To drive it directly:

    python3 check.py --account stnimbusdata [--port 4701]

The account must already exist; create one with Terraform
(examples/terraform/azure-storage) or `az storage account create`. The key is
read from the running server through ARM, so nothing is hard-coded here.
"""

import argparse
import json
import os
import ssl
import sys
import urllib.request

SUFFIX = "storage.nimbus.localhost"
SUBSCRIPTION = "00000000-0000-0000-0000-000000000000"


def ca_path() -> str:
    return os.path.join(os.path.expanduser("~"), ".nimbus", "certs", "nimbus-ca.pem")


def arm_get(url: str, ctx: ssl.SSLContext, method: str = "GET"):
    req = urllib.request.Request(url, method=method)
    with urllib.request.urlopen(req, context=ctx) as res:
        return json.load(res)


def account_key(port: int, account: str, ctx: ssl.SSLContext) -> str:
    """Find the account through ARM and ask for its key.

    Nimbus issues keys per account at creation, so they cannot be guessed --
    reading them back is exactly what a real deployment pipeline does.
    """
    base = f"https://localhost:{port}"
    listing = arm_get(
        f"{base}/subscriptions/{SUBSCRIPTION}/providers/Microsoft.Storage"
        f"/storageAccounts?api-version=2023-01-01",
        ctx,
    )
    for item in listing.get("value", []):
        if item["name"].lower() == account.lower():
            keys = arm_get(f"{base}{item['id']}/listKeys?api-version=2023-01-01", ctx, "POST")
            return keys["keys"][0]["value"]
    raise SystemExit(
        f"No storage account named {account!r}. Create one first, e.g. with\n"
        f"  cd examples/terraform/azure-storage && terraform apply"
    )


def step(name: str) -> None:
    print(f"  -> {name}", flush=True)


def check_blob(account: str, key: str, port: int, ca: str) -> None:
    from azure.storage.blob import BlobServiceClient

    endpoint = f"https://{account}.blob.{SUFFIX}:{port}"
    svc = BlobServiceClient(
        account_url=endpoint,
        credential={"account_name": account, "account_key": key},
        connection_verify=ca,
    )

    step("create container")
    name = "sdkcheck"
    if name in [c.name for c in svc.list_containers()]:
        svc.delete_container(name)
    container = svc.create_container(name)

    step("upload a small blob")
    container.upload_blob("hello.txt", b"hello nimbus", overwrite=True)

    step("upload 2 MB, which the SDK splits into blocks")
    big = os.urandom(2 * 1024 * 1024)
    container.upload_blob("big.bin", big, overwrite=True, max_concurrency=4)

    step("list blobs")
    listed = {b.name: b.size for b in container.list_blobs()}
    assert listed == {"hello.txt": 12, "big.bin": len(big)}, listed

    step("download and compare byte for byte")
    got = container.get_blob_client("big.bin").download_blob().readall()
    assert got == big, f"2 MB blob did not round-trip: {len(got)} bytes back"
    assert container.get_blob_client("hello.txt").download_blob().readall() == b"hello nimbus"

    step("read a byte range")
    part = container.get_blob_client("big.bin").download_blob(offset=100, length=50).readall()
    assert part == big[100:150], "range request returned the wrong bytes"

    step("set and read metadata")
    blob = container.get_blob_client("hello.txt")
    blob.set_blob_metadata({"owner": "nimbus", "kind": "greeting"})
    meta = blob.get_blob_properties().metadata
    assert meta.get("owner") == "nimbus" and meta.get("kind") == "greeting", meta

    step("list with a prefix and a delimiter")
    container.upload_blob("logs/a.txt", b"a", overwrite=True)
    container.upload_blob("logs/b.txt", b"b", overwrite=True)
    walked = list(container.walk_blobs(name_starts_with="logs/"))
    assert len(walked) == 2, [w.name for w in walked]

    step("delete a blob")
    blob.delete_blob()
    assert "hello.txt" not in [b.name for b in container.list_blobs()]

    step("wrong key is rejected")
    import base64

    bad = base64.b64encode(b"\x01" * 64).decode()
    bad_svc = BlobServiceClient(
        account_url=endpoint,
        credential={"account_name": account, "account_key": bad},
        connection_verify=ca,
    )
    try:
        list(bad_svc.list_containers())
    except Exception:
        pass  # expected
    else:
        raise AssertionError("a wrong account key was accepted")

    svc.delete_container(name)


def check_queue(account: str, key: str, port: int, ca: str) -> None:
    from azure.storage.queue import QueueServiceClient

    svc = QueueServiceClient(
        account_url=f"https://{account}.queue.{SUFFIX}:{port}",
        credential={"account_name": account, "account_key": key},
        connection_verify=ca,
    )
    name = "sdkcheck"
    if name in [q.name for q in svc.list_queues()]:
        svc.delete_queue(name)

    step("create queue and send messages")
    q = svc.create_queue(name)
    q.send_message("job-1")
    q.send_message("job-2")

    step("peek does not consume")
    assert [m.content for m in q.peek_messages(max_messages=5)] == ["job-1", "job-2"]
    assert len(list(q.receive_messages(messages_per_page=5))) == 2

    step("received messages are hidden until the visibility timeout lapses")
    assert list(q.receive_messages()) == []

    step("delete a message with its pop receipt")
    q.send_message("job-3")
    msg = next(iter(q.receive_messages()))
    q.delete_message(msg)

    svc.delete_queue(name)


def check_table(account: str, key: str, port: int, ca: str) -> None:
    from azure.core.credentials import AzureNamedKeyCredential
    from azure.data.tables import TableServiceClient

    svc = TableServiceClient(
        endpoint=f"https://{account}.table.{SUFFIX}:{port}",
        credential=AzureNamedKeyCredential(account, key),
        connection_verify=ca,
    )
    name = "sdkcheck"
    if name in [t.name for t in svc.list_tables()]:
        svc.delete_table(name)

    step("create table and insert entities")
    t = svc.create_table(name)
    t.create_entity({"PartitionKey": "eng", "RowKey": "ada", "role": "cto", "level": 5})
    t.create_entity({"PartitionKey": "eng", "RowKey": "bob", "role": "dev", "level": 3})
    t.create_entity({"PartitionKey": "ops", "RowKey": "cat", "role": "dev", "level": 3})

    step("query with an OData filter")
    rows = list(t.query_entities("PartitionKey eq 'eng' and role eq 'dev'"))
    assert [(r["PartitionKey"], r["RowKey"]) for r in rows] == [("eng", "bob")], rows

    step("merge leaves unnamed properties alone")
    t.upsert_entity({"PartitionKey": "eng", "RowKey": "ada", "role": "ceo"}, mode="merge")
    e = t.get_entity("eng", "ada")
    assert e["role"] == "ceo" and e["level"] == 5, dict(e)

    step("delete an entity")
    t.delete_entity("eng", "bob")
    assert len(list(t.list_entities())) == 2

    svc.delete_table(name)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--account", required=True, help="storage account name")
    ap.add_argument("--port", type=int, default=4701, help="Nimbus Azure port")
    args = ap.parse_args()

    ca = ca_path()
    if not os.path.exists(ca):
        print(f"FAIL: no CA at {ca}. Run `./bin/nimbus cert` first.", file=sys.stderr)
        return 1
    # Trust the Nimbus CA explicitly, so this works whether or not it has been
    # installed in the OS trust store.
    os.environ["SSL_CERT_FILE"] = ca
    ctx = ssl.create_default_context(cafile=ca)

    try:
        key = account_key(args.port, args.account, ctx)
    except urllib.error.URLError as err:
        print(f"FAIL: cannot reach Nimbus on port {args.port}: {err}", file=sys.stderr)
        print("Is `./bin/nimbus start` running?", file=sys.stderr)
        return 1

    for label, fn in (("BLOB", check_blob), ("QUEUE", check_queue), ("TABLE", check_table)):
        print(f"{label}")
        try:
            fn(args.account, key, args.port, ca)
        except AssertionError as err:
            print(f"FAIL ({label}): {err}", file=sys.stderr)
            return 1
        except Exception as err:  # noqa: BLE001 - report whatever the SDK raised
            print(f"FAIL ({label}): {type(err).__name__}: {err}", file=sys.stderr)
            return 1

    print("\nPASS: the Azure SDKs for Python work against Nimbus")
    return 0


if __name__ == "__main__":
    sys.exit(main())
