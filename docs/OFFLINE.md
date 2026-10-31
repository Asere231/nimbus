# Running fully offline

Nimbus itself never needs the network. Terraform does, once, to download the azurerm provider. Do this while online:

```bash
mkdir -p ~/.terraform-mirror
cd examples/terraform/azure-basic
terraform providers mirror ~/.terraform-mirror
```

Then tell Terraform to use only the mirror. Create `~/.terraformrc` (Windows: `%APPDATA%\terraform.rc`):

```hcl
provider_installation {
  filesystem_mirror {
    path    = "/Users/YOU/.terraform-mirror"   # Windows: "C:/Users/YOU/.terraform-mirror"
    include = ["registry.terraform.io/hashicorp/*"]
  }
  direct {
    exclude = ["registry.terraform.io/hashicorp/*"]
  }
}
```

From Milestone 3 (AKS on k3s), also pre-pull the k3s image while online. The
tag is pinned in `internal/azure/server.go` (`DefaultK3sImage`) and matched to
a kubectl minor version, so keep the two in step if you change it:

```bash
docker pull rancher/k3s:v1.34.12-k3s1
```
