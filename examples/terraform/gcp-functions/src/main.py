"""An HTTP Cloud Function, written the way one is written for real.

There is nothing Nimbus-specific here. The server that runs it is Google's
own Functions Framework -- an ordinary pip package your source carries -- so
the programming model is the real one: a function taking a Flask request and
returning a response, registered with the framework's decorator.

That is the third of three completely different answers to "how does a cloud
run my code":

  Lambda            the base image ships the runtime client; the code is
                    MOUNTED into it
  Azure Functions   a .NET host negotiates with a language worker over gRPC,
                    and only finds the code if it is BAKED IN
  Cloud Functions   the contract is a LIBRARY you bring, served by an
                    ordinary HTTP server reading $PORT

The third asks the least of the platform, which is why Nimbus runs it on the
same engine that runs Cloud Run and Azure Container Apps.
"""

import json
import os

import functions_framework


@functions_framework.http
def hello(request):
    name = request.args.get("name") or "world"

    # Both of these reach the container's console, which is where
    # `docker logs` reads them from.
    print(f"hello called for {name}")

    return (
        json.dumps(
            {
                "greeting": f"hello {name} from a cloud function",
                # The framework learns the port from $PORT, which the engine
                # sets -- no cloud-specific plumbing anywhere.
                "port": os.environ.get("PORT"),
                # Set from buildConfig.entryPoint, so a handler can see which
                # target it was deployed as.
                "target": os.environ.get("FUNCTION_TARGET"),
            }
        )
        + "\n",
        200,
        {"Content-Type": "application/json"},
    )
