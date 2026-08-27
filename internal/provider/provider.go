// Package provider is the Terraform provider for Firstboot.
//
// It is deliberately thin. Everything that is hard about talking to this API --
// knowing when a create has finished, retrying without buying a second server,
// reading a rate limit's own answer -- lives in the firstboot-go SDK, because a
// CLI and an MCP server need the same three things and would otherwise each
// implement them differently. What is left here is the part that is genuinely
// Terraform's: schemas, plan modifiers, and the mapping between a state file and
// a resource.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	firstboot "github.com/firstboot-io/firstboot-go"
)

// Ensure the implementation satisfies the interfaces the framework dispatches
// on. Compile-time rather than a comment, because a missing method surfaces as
// "provider does not support resources" at plan time otherwise.
var _ provider.Provider = (*firstbootProvider)(nil)

type firstbootProvider struct {
	// version is stamped by the release build and reported to the API in the
	// User-Agent. "which provider version is doing this" is otherwise
	// unanswerable from the platform's side.
	version string
}

// New is the entry point main.go serves.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &firstbootProvider{version: version} }
}

func (p *firstbootProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "firstboot"
	resp.Version = p.version
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

func (p *firstbootProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages Firstboot cloud resources: servers, container apps, managed " +
			"databases, volumes, private networks, firewalls, load balancers, floating IPs, " +
			"DNS, reverse DNS, custom ISOs and domain registrations.\n\n" +
			"An API token is pinned to one organization for its whole life, so a provider " +
			"configuration IS an organization. Managing two organizations means two provider " +
			"blocks with an alias, not a per-resource argument.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "API base URL. Defaults to the `FIRSTBOOT_API_URL` environment variable.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "API token (`pat_…`), created under Account settings → API keys. " +
					"Defaults to the `FIRSTBOOT_TOKEN` environment variable.\n\n" +
					"Give it the narrowest scopes the configuration needs. `destroy` is the one " +
					"to think about: without it `terraform destroy` fails, and with it a mistaken " +
					"apply can remove a machine.",
			},
		},
	}
}

func (p *firstbootProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown value at this point means the configuration takes it from
	// another resource's output, which cannot be resolved before the provider
	// is configured. Saying so beats a nil-pointer panic three steps later.
	if cfg.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("token"),
			"Token is not known at plan time",
			"The provider's token cannot come from a resource attribute, because the provider "+
				"is configured before any resource is read. Use a variable or the FIRSTBOOT_TOKEN "+
				"environment variable.")
		return
	}
	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"),
			"Endpoint is not known at plan time",
			"The provider's endpoint cannot come from a resource attribute. Use a variable or "+
				"the FIRSTBOOT_API_URL environment variable.")
		return
	}

	opts := []firstboot.Option{
		firstboot.WithUserAgent("terraform-provider-firstboot/" + p.version),
	}
	if v := cfg.Endpoint.ValueString(); v != "" {
		opts = append(opts, firstboot.WithBaseURL(v))
	}
	if v := cfg.Token.ValueString(); v != "" {
		opts = append(opts, firstboot.WithToken(v))
	}

	client, err := firstboot.New(opts...)
	if err != nil {
		// The two ways this fails are both configuration, and both are worth
		// naming rather than passing the SDK's sentence through: an operator
		// reading "no API token" wants to be told where Terraform looks for one.
		detail := err.Error()
		if os.Getenv(firstboot.EnvToken) == "" && cfg.Token.ValueString() == "" {
			detail += "\n\nSet the `token` argument on the provider block, or the " +
				firstboot.EnvToken + " environment variable."
		}
		if os.Getenv(firstboot.EnvBaseURL) == "" && cfg.Endpoint.ValueString() == "" {
			detail += "\n\nSet the `endpoint` argument on the provider block, or the " +
				firstboot.EnvBaseURL + " environment variable."
		}
		resp.Diagnostics.AddError("Cannot configure the Firstboot client", detail)
		return
	}

	// Both, because a data source is configured from DataSourceData and a
	// resource from ResourceData, and supplying only one is a nil client in
	// whichever half was forgotten.
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *firstbootProvider) Resources(_ context.Context) []func() resource.Resource {
	// Registered here means implemented. A provider that advertises a resource
	// it cannot serve fails at apply with a nil-pointer panic rather than with
	// "unsupported resource type", which is the worse of the two by a long way.
	return []func() resource.Resource{
		NewServerResource,
		NewSSHKeyResource,
		NewProjectResource,
		NewVolumeResource,
		NewNetworkResource,
		NewFirewallResource,
		NewFloatingIPResource,
		NewDNSZoneResource,
		NewDNSRecordResource,
		NewLoadBalancerResource,
		NewDatabaseResource,
		NewAppResource,
		NewISOResource,
		NewDomainResource,
		NewRDNSResource,
	}
}

func (p *firstbootProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return append([]func() datasource.DataSource{
		NewPlansDataSource,
		NewRegionsDataSource,
		NewImagesDataSource,
	},
		// The eight plural selectors: firstboot_servers, firstboot_volumes and
		// so on. They exist so a configuration can name a fleet by what it IS
		// rather than by listing ids; see data_source_grouped.go.
		GroupedDataSources()...)
}
