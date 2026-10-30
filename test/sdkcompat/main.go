// sdkcompat drives a running Nimbus with hashicorp/go-azure-sdk, the same
// SDK the Terraform azurerm provider uses: environment discovery from
// metadata_host, client-secret auth, PUT + provisioning-state polling,
// paged lists, and DELETE + delete polling.
//
//	go run . -endpoint https://localhost:4701
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/hashicorp/go-azure-sdk/sdk/auth"
	"github.com/hashicorp/go-azure-sdk/sdk/client"
	"github.com/hashicorp/go-azure-sdk/sdk/client/resourcemanager"
	"github.com/hashicorp/go-azure-sdk/sdk/environments"
)

const (
	subID  = "00000000-0000-0000-0000-000000000000"
	tenant = "00000000-0000-0000-0000-000000000001"
)

func main() {
	endpoint := flag.String("endpoint", "https://localhost:4701", "Nimbus Azure endpoint")
	flag.Parse()
	if err := run(*endpoint); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS: go-azure-sdk works against Nimbus")
}

func step(name string) { fmt.Println("  ->", name) }

func run(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	step("environment from metadata endpoint")
	env, err := environments.FromEndpoint(ctx, endpoint)
	if err != nil {
		return err
	}

	step("client secret token")
	authorizer, err := auth.NewClientSecretAuthorizer(ctx, auth.ClientSecretAuthorizerOptions{
		Environment: *env, Api: env.ResourceManager, TenantId: tenant,
		ClientId: "00000000-0000-0000-0000-000000000002", ClientSecret: "nimbus",
	})
	if err != nil {
		return err
	}

	c, err := resourcemanager.NewClient(env.ResourceManager, "nimbus-sdkcompat", "2023-09-01")
	if err != nil {
		return err
	}
	c.Authorizer = authorizer

	rg := "/subscriptions/" + subID + "/resourceGroups/rg-sdkcompat"
	vnet := rg + "/providers/Microsoft.Network/virtualNetworks/vnet-sdk"
	subnet := vnet + "/subnets/app"

	step("create resource group")
	if err := put(ctx, c, rg, map[string]any{"location": "eastus"}, false); err != nil {
		return err
	}
	step("create virtual network (long-running operation)")
	if err := put(ctx, c, vnet, map[string]any{"location": "eastus", "properties": map[string]any{
		"addressSpace": map[string]any{"addressPrefixes": []string{"10.10.0.0/16"}}}}, true); err != nil {
		return err
	}
	step("create subnet (long-running operation)")
	if err := put(ctx, c, subnet, map[string]any{"properties": map[string]any{"addressPrefix": "10.10.1.0/24"}}, true); err != nil {
		return err
	}

	step("read virtual network")
	var got struct {
		Properties struct {
			Subnets []struct{ Name string } `json:"subnets"`
		} `json:"properties"`
	}
	if err := get(ctx, c, vnet, &got); err != nil {
		return err
	}
	if len(got.Properties.Subnets) != 1 || got.Properties.Subnets[0].Name != "app" {
		return fmt.Errorf("vnet should embed subnet 'app', got %+v", got.Properties.Subnets)
	}

	step("paged list of subnets")
	req, err := c.NewRequest(ctx, client.RequestOptions{ContentType: "application/json; charset=utf-8", ExpectedStatusCodes: []int{http.StatusOK}, HttpMethod: http.MethodGet, Path: vnet + "/subnets"})
	if err != nil {
		return err
	}
	resp, err := req.ExecutePaged(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	var page struct{ Value []map[string]any }
	if err := resp.Unmarshal(&page); err != nil || len(page.Value) != 1 {
		return fmt.Errorf("list subnets: %v %d", err, len(page.Value))
	}

	step("delete subnet, network, resource group (delete pollers)")
	for _, id := range []string{subnet, vnet, rg} {
		if err := del(ctx, c, id); err != nil {
			return err
		}
	}
	step("confirm resource group is gone")
	if err := get(ctx, c, rg, &map[string]any{}); err == nil {
		return fmt.Errorf("resource group still exists")
	}
	return nil
}

func put(ctx context.Context, c *resourcemanager.Client, path string, body any, poll bool) error {
	req, err := c.NewRequest(ctx, client.RequestOptions{ContentType: "application/json; charset=utf-8", ExpectedStatusCodes: []int{http.StatusOK, http.StatusCreated}, HttpMethod: http.MethodPut, Path: path})
	if err != nil {
		return err
	}
	if err := req.Marshal(body); err != nil {
		return err
	}
	resp, err := req.Execute(ctx)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	if !poll {
		return nil
	}
	poller, err := resourcemanager.PollerFromResponse(resp, c)
	if err != nil {
		return fmt.Errorf("poller for %s: %w", path, err)
	}
	return poller.PollUntilDone(ctx)
}

func get(ctx context.Context, c *resourcemanager.Client, path string, out any) error {
	req, err := c.NewRequest(ctx, client.RequestOptions{ContentType: "application/json; charset=utf-8", ExpectedStatusCodes: []int{http.StatusOK}, HttpMethod: http.MethodGet, Path: path})
	if err != nil {
		return err
	}
	resp, err := req.Execute(ctx)
	if err != nil {
		return err
	}
	return resp.Unmarshal(out)
}

func del(ctx context.Context, c *resourcemanager.Client, path string) error {
	req, err := c.NewRequest(ctx, client.RequestOptions{ContentType: "application/json; charset=utf-8", ExpectedStatusCodes: []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent}, HttpMethod: http.MethodDelete, Path: path})
	if err != nil {
		return err
	}
	resp, err := req.Execute(ctx)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	poller, err := resourcemanager.PollerFromResponse(resp, c)
	if err != nil {
		return fmt.Errorf("delete poller for %s: %w", path, err)
	}
	return poller.PollUntilDone(ctx)
}
