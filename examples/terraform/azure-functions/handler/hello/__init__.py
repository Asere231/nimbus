"""An HTTP-triggered Azure Function, written the way one is written for real.

There is nothing Nimbus-specific in here. The Functions host that runs it
is Microsoft's own image, so the programming model is the real one: a
`main` taking an HttpRequest and returning an HttpResponse, with the
trigger declared in function.json beside it.

The classic model (a folder per function with a function.json) is used
rather than the v2 decorators because the HOST indexes it directly, where
the v2 model is indexed by the Python worker. Both were measured against
Nimbus and both work; this one has one less moving part to explain.
"""

import logging

import azure.functions as func


def main(req: func.HttpRequest) -> func.HttpResponse:
    name = req.params.get("name")
    if not name:
        # A POST body is the other way a caller passes a name, and
        # get_json raises rather than returning None when there is none.
        try:
            name = (req.get_json() or {}).get("name")
        except ValueError:
            name = None
    name = name or "world"

    # Both of these reach the container's console, which is where
    # `docker logs` reads them from -- and the host's own
    # "Executed 'Functions.hello' (Succeeded...)" line appears beside
    # them, so the console shows the invocation as well as its output.
    print(f"hello called for {name}")
    logging.info("logging works too: %s", name)

    return func.HttpResponse(f"hello {name} from azure functions", status_code=200)
