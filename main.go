// Command terraform-provider-firstboot serves the Firstboot provider over
// Terraform's plugin protocol.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/firstboot-io/terraform-provider-firstboot/internal/provider"
)

// version is stamped by the release build (-ldflags "-X main.version=…"). It
// reaches the API in the User-Agent, which is what makes "one provider version
// is doing something odd" answerable from the platform's side.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false,
		"run with support for a debugger, printing the reattach configuration Terraform needs")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// The registry address, not a URL. Terraform resolves
		// `firstboot-io/firstboot` in a required_providers block to this.
		Address: "registry.terraform.io/firstboot-io/firstboot",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
