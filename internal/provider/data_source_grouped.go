package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	firstboot "github.com/firstboot-io/firstboot-go"
)

// The plural data sources: SELECT resources by what they are, not by listing
// their ids.
//
// This is what tags exist for. A fleet built with `count = 3` should be handed
// to a load balancer by role rather than enumerated, and enumerating is what a
// configuration has had to do until now:
//
//	data "firstboot_servers" "web" {
//	  tags = ["role:web"]
//	}
//
//	resource "firstboot_load_balancer" "lb" {
//	  backend_ids = data.firstboot_servers.web.ids
//	}
//
// Eight of them, one per groupable kind, from one table for the reason the
// platform's own `grouping.Resources` is a table: eight hand-written data
// sources are eight chances for one of them to filter differently.
//
// They answer with IDS AND NAMES and nothing else, deliberately. A data source
// that mirrored each resource's full shape would be eight more schemas to keep
// in step with eight resources, and the question these answer is "which ones",
// not "what are they". Reading one is `terraform import` or the resource
// itself.

// groupedKind is one row of that table.
type groupedKind struct {
	// Name is the type suffix: "servers" -> firstboot_servers.
	Name string
	// Noun is the singular, for the description.
	Noun string
	// List walks the kind with the two filters applied by the API.
	List func(context.Context, *firstboot.Client, []string, string) ([]groupedItem, error)
}

type groupedItem struct {
	ID   string
	Name string
}

var groupedKinds = []groupedKind{
	{"servers", "server", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.ServerListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.ServersWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.ServersInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Servers(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
	{"volumes", "volume", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.VolumeListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.VolumesWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.VolumesInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Volumes(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
	{"networks", "private network", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.NetworkListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.NetworksWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.NetworksInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Networks(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
	{"databases", "managed database", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.DatabaseListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.DatabasesWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.DatabasesInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Databases(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
	{"load_balancers", "load balancer", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.LoadBalancerListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.LoadBalancersWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.LoadBalancersInProject(project))
		}
		var out []groupedItem
		for v, err := range c.LoadBalancers(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
	{"dns_zones", "DNS zone", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.ZoneListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.ZonesWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.ZonesInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Zones(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
	// An app's id IS its code, the same value `firstboot_app.id` carries, so a
	// configuration can hand these straight to anything that names an app.
	{"apps", "app", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.AppListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.AppsWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.AppsInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Apps(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Code, Name: v.Name})
		}
		return out, nil
	}},
	{"domains", "domain", func(ctx context.Context, c *firstboot.Client, tags []string, project string) ([]groupedItem, error) {
		var opts []firstboot.DomainListOption
		if len(tags) > 0 {
			opts = append(opts, firstboot.DomainsWithTags(tags...))
		}
		if project != "" {
			opts = append(opts, firstboot.DomainsInProject(project))
		}
		var out []groupedItem
		for v, err := range c.Domains(ctx, opts...) {
			if err != nil {
				return nil, err
			}
			out = append(out, groupedItem{ID: v.Id, Name: v.Name})
		}
		return out, nil
	}},
}

// GroupedDataSources is every plural data source, for the provider's registry.
func GroupedDataSources() []func() datasource.DataSource {
	out := make([]func() datasource.DataSource, 0, len(groupedKinds))
	for _, k := range groupedKinds {
		out = append(out, func() datasource.DataSource { return &groupedDataSource{kind: k} })
	}
	return out
}

type groupedDataSource struct {
	datasourceConfigure
	kind groupedKind
}

type groupedDataSourceModel struct {
	Tags      types.Set    `tfsdk:"tags"`
	ProjectID types.String `tfsdk:"project_id"`
	IDs       types.List   `tfsdk:"ids"`
	Names     types.List   `tfsdk:"names"`
}

func (d *groupedDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + d.kind.Name
}

func (d *groupedDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dschema.Schema{
		MarkdownDescription: "Selects " + d.kind.Noun + "s by tag or by project.\n\n" +
			"Both filters are applied by the API before paging, so this narrows the whole " +
			"organization rather than one page. With neither, it answers with every " +
			d.kind.Noun + ".\n\n" +
			"`ids` and `names` are in the same order, so `element(names, index(ids, x))` " +
			"resolves one from the other.",
		Attributes: map[string]dschema.Attribute{
			"tags": dschema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Match " + d.kind.Noun + "s carrying EVERY tag listed. " +
					"Two tags mean both, not either: the API's filter is a containment test.",
			},
			"project_id": dschema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Match " + d.kind.Noun + "s in this project, or the " +
					"literal `none` for the ones in no project at all. Those are different " +
					"questions and a UUID can only ask the first.",
			},
			"ids": dschema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The matching ids, oldest first. Hand these to anything " +
					"that takes a list of " + d.kind.Noun + "s.",
			},
			"names": dschema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The matching names, in the same order as `ids`.",
			},
		},
	}
}

func (d *groupedDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg groupedDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var tags []string
	if !cfg.Tags.IsNull() && !cfg.Tags.IsUnknown() {
		resp.Diagnostics.Append(cfg.Tags.ElementsAs(ctx, &tags, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	items, err := d.kind.List(ctx, d.client, tags, cfg.ProjectID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Selecting "+d.kind.Noun+"s", err)
		return
	}

	ids := make([]string, 0, len(items))
	names := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
		names = append(names, it.Name)
	}
	idList, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	nameList, diags := types.ListValueFrom(ctx, types.StringType, names)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	cfg.IDs, cfg.Names = idList, nameList
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
