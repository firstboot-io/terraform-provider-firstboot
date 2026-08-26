package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/firstboot-go/fbapi"
)

// The catalog reads. All three are unauthenticated on the API's side, which is
// what lets a configuration discover what it can ask for before it asks: the
// alternative is hardcoding a plan slug and finding out at apply time that the
// region does not sell it.

var (
	_ datasource.DataSource              = (*plansDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*plansDataSource)(nil)
	_ datasource.DataSource              = (*regionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*regionsDataSource)(nil)
	_ datasource.DataSource              = (*imagesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*imagesDataSource)(nil)
)

// ---------- plans ----------

func NewPlansDataSource() datasource.DataSource { return &plansDataSource{} }

type plansDataSource struct{ datasourceConfigure }

type planModel struct {
	Slug              types.String `tfsdk:"slug"`
	Name              types.String `tfsdk:"name"`
	Cores             types.Int64  `tfsdk:"cores"`
	MemoryMB          types.Int64  `tfsdk:"memory_mb"`
	DiskGB            types.Int64  `tfsdk:"disk_gb"`
	TrafficGB         types.Int64  `tfsdk:"traffic_gb"`
	MonthlyPriceMinor types.Int64  `tfsdk:"monthly_price_minor"`
	Currency          types.String `tfsdk:"currency"`
}

type plansModel struct {
	Plans []planModel `tfsdk:"plans"`
}

func (d *plansDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_plans"
}

func (d *plansDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every server plan on sale.\n\n" +
			"Prices are in MINOR units (kuruş or cents) of the currency named beside them, " +
			"resolved into the organization's own currency. There is no floating-point money " +
			"here, deliberately: `monthly_price_minor = 12000` with `currency = \"TRY\"` is " +
			"120.00 TL and cannot round to something else.",
		Attributes: map[string]schema.Attribute{
			"plans": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"slug":                schema.StringAttribute{Computed: true, MarkdownDescription: "The value a `firstboot_server`'s `plan` takes."},
						"name":                schema.StringAttribute{Computed: true},
						"cores":               schema.Int64Attribute{Computed: true},
						"memory_mb":           schema.Int64Attribute{Computed: true},
						"disk_gb":             schema.Int64Attribute{Computed: true},
						"traffic_gb":          schema.Int64Attribute{Computed: true, MarkdownDescription: "Included monthly transfer; past it, overage is billed."},
						"monthly_price_minor": schema.Int64Attribute{Computed: true},
						"currency":            schema.StringAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *plansDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	out, err := d.client.API.PlansListWithResponse(ctx, &fbapi.PlansListParams{})
	if err != nil {
		apiError(&resp.Diagnostics, "Reading plans", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Reading plans",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	var m plansModel
	if out.JSON200.Plans != nil {
		for _, p := range *out.JSON200.Plans {
			m.Plans = append(m.Plans, planModel{
				Slug:              types.StringValue(p.Slug),
				Name:              types.StringValue(p.Name),
				Cores:             types.Int64Value(p.Cores),
				MemoryMB:          types.Int64Value(p.MemoryMb),
				DiskGB:            types.Int64Value(p.DiskGb),
				TrafficGB:         types.Int64Value(p.TrafficGb),
				MonthlyPriceMinor: types.Int64Value(p.MonthlyPriceMinor),
				Currency:          types.StringValue(p.Currency),
			})
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---------- regions ----------

func NewRegionsDataSource() datasource.DataSource { return &regionsDataSource{} }

type regionsDataSource struct{ datasourceConfigure }

type regionModel struct {
	Slug types.String `tfsdk:"slug"`
	Name types.String `tfsdk:"name"`
}

type regionsModel struct {
	Regions []regionModel `tfsdk:"regions"`
}

func (d *regionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every region a resource can be created in.\n\n" +
			"There is no live migration between regions: moving a server means a new machine " +
			"with a new address. Choosing the region is a decision made once.",
		Attributes: map[string]schema.Attribute{
			"regions": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"slug": schema.StringAttribute{Computed: true},
						"name": schema.StringAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *regionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	out, err := d.client.API.RegionsListWithResponse(ctx)
	if err != nil {
		apiError(&resp.Diagnostics, "Reading regions", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Reading regions",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	var m regionsModel
	if out.JSON200.Regions != nil {
		for _, r := range *out.JSON200.Regions {
			m.Regions = append(m.Regions, regionModel{
				Slug: types.StringValue(r.Slug),
				Name: types.StringValue(r.Name),
			})
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---------- images ----------

func NewImagesDataSource() datasource.DataSource { return &imagesDataSource{} }

type imagesDataSource struct{ datasourceConfigure }

type imageModel struct {
	Slug      types.String `tfsdk:"slug"`
	Name      types.String `tfsdk:"name"`
	MinDiskGB types.Int64  `tfsdk:"min_disk_gb"`
}

type imagesModel struct {
	Images []imageModel `tfsdk:"images"`
}

func (d *imagesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images"
}

func (d *imagesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every image a server can be created from: operating systems and " +
			"one-click applications.\n\n" +
			"`min_disk_gb` is worth reading rather than assuming: an image whose minimum exceeds " +
			"the plan's disk is refused at create time.",
		Attributes: map[string]schema.Attribute{
			"images": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"slug":        schema.StringAttribute{Computed: true, MarkdownDescription: "The value a `firstboot_server`'s `image` takes."},
						"name":        schema.StringAttribute{Computed: true},
						"min_disk_gb": schema.Int64Attribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *imagesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	out, err := d.client.API.ImagesListWithResponse(ctx, &fbapi.ImagesListParams{})
	if err != nil {
		apiError(&resp.Diagnostics, "Reading images", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Reading images",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	var m imagesModel
	if out.JSON200.Images != nil {
		for _, i := range *out.JSON200.Images {
			m.Images = append(m.Images, imageModel{
				Slug:      types.StringValue(i.Slug),
				Name:      types.StringValue(i.Name),
				MinDiskGB: types.Int64Value(i.MinDiskGb),
			})
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
