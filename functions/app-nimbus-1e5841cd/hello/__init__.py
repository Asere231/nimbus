import azure.functions as func


def main(req: func.HttpRequest) -> func.HttpResponse:
    name = req.params.get("name", "world")
    print(f"hello called for {name}")
    return func.HttpResponse(f"hello {name} from azure functions", status_code=200)
