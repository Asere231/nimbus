# Bicep / ARM template example

A VNet with two subnets, a network security group they all point at, and a
storage account whose name comes from `uniqueString()`.

```bash
./bin/nimbus start                                   # terminal 1

# terminal 2, once per machine:
export REQUESTS_CA_BUNDLE=~/.nimbus/certs/nimbus-ca.pem
./bin/nimbus az register --run
az login --service-principal -u 00000000-0000-0000-0000-000000000002 -p nimbus \
  --tenant 00000000-0000-0000-0000-000000000001 --allow-no-subscriptions

az group create -n rg-bicep -l eastus
az deployment group create -g rg-bicep --template-file main.bicep
```

Watch <http://localhost:4700> while it runs: each resource appears as the
deployment reaches it, because a deployed resource goes through exactly the
same path as one Terraform creates.

Then:

```bash
az deployment group what-if -g rg-bicep --template-file main.bicep   # no changes
az resource list -g rg-bicep -o table
```

## Why there are two files

`main.bicep` is the source. `main.json` is what it compiles to, and it is
committed because **that is what Nimbus actually receives**: `az deployment
group create --template-file main.bicep` runs the Bicep compiler on your
machine and sends ARM JSON. Nimbus has no Bicep parser and needs none.

CI deploys `main.json` directly, so the example is covered without installing
the Bicep compiler. If you edit `main.bicep`, regenerate it:

```bash
az bicep install          # once; downloads the compiler
az bicep build --file examples/bicep/main.bicep
```

## What it exercises

- `parameters()`, `variables()`, `resourceId()`, `resourceGroup()`,
  `format()`, `length()`, `range()`, `toLower()` and `reference()`
- a **property-level** copy loop — what a Bicep `for` inside a property
  compiles to, producing the `subnets` array
- `dependsOn`, plus the implicit dependency the subnets' `networkSecurityGroup.id`
  creates
- outputs, including one that reads a deployed resource back with `reference()`

## One difference from real Azure

`uniqueString()` is deterministic and stable here, but it is **not** Azure's
algorithm, which is unpublished. The storage account name this template
produces against Nimbus differs from the one real Azure would produce. A
template that only needs a unique, repeatable name — like this one — behaves
identically either way.
