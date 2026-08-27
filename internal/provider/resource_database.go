package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/firstboot-go/fbapi"
)

var (
	_ resource.Resource                = (*databaseResource)(nil)
	_ resource.ResourceWithConfigure   = (*databaseResource)(nil)
	_ resource.ResourceWithImportState = (*databaseResource)(nil)
)

func NewDatabaseResource() resource.Resource { return &databaseResource{} }

type databaseResource struct{ resourceConfigure }

type dbTrustedSourceModel struct {
	CIDR types.String `tfsdk:"cidr"`
	Note types.String `tfsdk:"note"`
}

type databaseModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Engine        types.String `tfsdk:"engine"`
	EngineVersion types.String `tfsdk:"engine_version"`
	Plan          types.String `tfsdk:"plan"`
	Region        types.String `tfsdk:"region"`
	NetworkID     types.String `tfsdk:"network_id"`
	ProjectID     types.String `tfsdk:"project_id"`
	Tags          types.Set    `tfsdk:"tags"`
	PublicAccess  types.Bool   `tfsdk:"public_access"`
	WaitFor       types.Bool   `tfsdk:"wait_for_ready"`

	TrustedSources []dbTrustedSourceModel `tfsdk:"trusted_source"`

	Code         types.String `tfsdk:"code"`
	IP           types.String `tfsdk:"ip"`
	State        types.String `tfsdk:"state"`
	PendingApply types.Bool   `tfsdk:"pending_apply"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func (r *databaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (r *databaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed PostgreSQL or MySQL instance.\n\n" +
			"This resource manages the INSTANCE. The databases and users inside it are not " +
			"Terraform resources here: their credentials are returned once, by an endpoint an " +
			"API token is deliberately not allowed to call, so a provider that managed them " +
			"would either fail or write passwords into the state file. Create them from the " +
			"panel, or from the application's own migration step.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The instance's name. The API has no rename, so a change " +
					"replaces the instance, which DESTROYS its data.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"engine": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`postgresql` (the default) or `mysql`. There is no conversion " +
					"between the two, so a change replaces the instance and destroys its data.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"engine_version": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The pinned major version; defaults to the catalog's current one.\n\n" +
					"**Never upgraded automatically, and not upgradeable in place.** A change here " +
					"replaces the instance and destroys its data; a real major upgrade is a dump " +
					"and a restore into a second instance, which is a migration rather than a plan.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"plan": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`db-1`, `db-2` or `db-3`; defaults to `db-1`. Changed in place.\n\n" +
					"**Grow only.** The API refuses a plan that shrinks the disk or lowers the " +
					"price, so a downgrade fails at apply rather than at plan.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Region slug; defaults to the platform's first active region.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"project_id": projectAttribute("database"),
			"tags":       tagsAttribute("database"),
			"network_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional private network. Servers on it reach the instance " +
					"privately, which is what lets `public_access` be turned off.\n\n" +
					"The API has no endpoint to move an instance between networks, so a change " +
					"replaces it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"public_access": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the instance answers on its public address. Changed in place.\n\n" +
					"With it on, `trusted_source` is what stands between the instance and the " +
					"internet: an empty list means nothing is allowed in, not that everything is.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the instance to finish provisioning.",
			},

			"code": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The short public identity, which is also the instance's DNS label. " +
					"Stable for its whole life.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"ip": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The public address, assigned during provisioning.",
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`active`, `stopped_dunning`, `suspended` and `deleted` are settled; " +
					"an `error_*` value means provisioning failed.",
			},
			"pending_apply": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "True while an edit has not reached the appliance yet. A trusted " +
					"source or a public-access change leaves the state `active` throughout, so this " +
					"is the only field that moves.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
		Blocks: map[string]schema.Block{
			"trusted_source": schema.ListNestedBlock{
				MarkdownDescription: "An address or CIDR allowed to connect. The whole set is replaced " +
					"on every change, which is what the API offers and what keeps two concurrent " +
					"edits from losing entries.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"cidr": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "An address or a CIDR. A plain address is canonicalised " +
								"to `/32`, so `203.0.113.4` and `203.0.113.4/32` are the same entry -- " +
								"write the form the API returns or every plan shows a change.",
						},
						"note": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "A label for whoever reads the list later.",
						},
					},
				},
			},
		},
	}
}

func (r *databaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := fbapi.DatabaseCreateInputBody{Name: plan.Name.ValueString()}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	body.Tags = tagsFromPlan(ctx, plan.Tags, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if v := plan.Engine.ValueString(); v != "" && !plan.Engine.IsUnknown() {
		e := fbapi.DatabaseCreateInputBodyEngine(v)
		body.Engine = &e
	}
	if v := plan.EngineVersion.ValueString(); v != "" && !plan.EngineVersion.IsUnknown() {
		body.EngineVersion = &v
	}
	if v := plan.Plan.ValueString(); v != "" && !plan.Plan.IsUnknown() {
		body.Plan = &v
	}
	if v := plan.Region.ValueString(); v != "" && !plan.Region.IsUnknown() {
		body.Region = &v
	}
	if v := plan.NetworkID.ValueString(); v != "" {
		body.NetworkId = &v
	}

	out, err := r.client.API.DatabaseCreateWithResponse(ctx, &fbapi.DatabaseCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the database", err)
		return
	}
	if out.JSON202 == nil {
		apiError(&resp.Diagnostics, "Creating the database",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	id := out.JSON202.Id
	applyDatabase(ctx, &plan, out.JSON202, nil, &resp.Diagnostics)
	// State before the wait, and before the two settings below: an interrupted
	// apply must not leave a billing instance that no state file names.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Public access and trusted sources are their own endpoints, so a create
	// that configures them is a create plus two edits. They run AFTER the wait
	// rather than before it: the appliance has to exist before it can be told
	// who may reach it, and an edit sent at `provisioning` is refused.
	if plan.WaitFor.ValueBool() {
		if _, err := r.client.WaitForDatabase(ctx, id); err != nil {
			waitError(&resp.Diagnostics, "Waiting for the database", err)
			return
		}
	}
	if !plan.PublicAccess.IsUnknown() && !plan.PublicAccess.IsNull() {
		if !r.setPublicAccess(ctx, id, plan.PublicAccess.ValueBool(), &resp.Diagnostics) {
			return
		}
	}
	if len(plan.TrustedSources) > 0 {
		if !r.setTrustedSources(ctx, id, plan.TrustedSources, &resp.Diagnostics) {
			return
		}
	}
	if !r.refresh(ctx, id, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *databaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DatabaseGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the database", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the database",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyDatabase(ctx, &state, &out.JSON200.Database, out.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	if !applyGrouping(ctx, &resp.Diagnostics, groupingUpdate{
		Noun: "database", PlanTags: plan.Tags, StateTags: state.Tags,
		PlanProject: plan.ProjectID, StateProject: state.ProjectID,
		SetTags: func(ctx context.Context, b fbapi.TagsBody) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.DatabaseTagsSetWithResponse(ctx, id, b)
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
		SetProject: func(ctx context.Context, pid *string) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.DatabaseProjectSetWithResponse(ctx, id,
				fbapi.DatabaseProjectSetJSONRequestBody{ProjectId: pid})
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
	}) {
		return
	}

	// Trusted sources before public access, so turning the public address on
	// never exposes an instance to a list that has not been narrowed yet. The
	// reverse order has a window, however short, where the old list is live on
	// a newly public address.
	if !dbSourcesEqual(plan.TrustedSources, state.TrustedSources) {
		if !r.setTrustedSources(ctx, id, plan.TrustedSources, &resp.Diagnostics) {
			return
		}
	}
	if !plan.PublicAccess.Equal(state.PublicAccess) {
		if !r.setPublicAccess(ctx, id, plan.PublicAccess.ValueBool(), &resp.Diagnostics) {
			return
		}
	}

	if !plan.Plan.Equal(state.Plan) {
		out, err := r.client.API.DatabaseResizeWithResponse(ctx, id,
			fbapi.DatabaseResizeInputBody{Plan: plan.Plan.ValueString()})
		if err != nil {
			apiError(&resp.Diagnostics, "Resizing the database", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Resizing the database",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
		// A resize moves the state to `resizing`, so a read that did not wait
		// would write that transient value into state as the settled answer.
		if plan.WaitFor.ValueBool() {
			if _, err := r.client.WaitForDatabase(ctx, id); err != nil {
				waitError(&resp.Diagnostics, "Waiting for the resize", err)
				return
			}
		}
	}

	if !r.refresh(ctx, id, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *databaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DatabaseDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the database", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the database",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *databaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *databaseResource) setPublicAccess(ctx context.Context, id string, enabled bool, diags *diagSink) bool {
	out, err := r.client.API.DatabasePublicAccessSetWithResponse(ctx, id,
		fbapi.DatabasePublicAccessInputBody{Enabled: enabled})
	if err != nil {
		apiError(diags, "Setting the database's public access", err)
		return false
	}
	if out.StatusCode() >= 400 {
		apiError(diags, "Setting the database's public access",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	return true
}

func (r *databaseResource) setTrustedSources(ctx context.Context, id string, sources []dbTrustedSourceModel, diags *diagSink) bool {
	list := make([]fbapi.DatabaseTrustedSourceBody, 0, len(sources))
	for _, s := range sources {
		e := fbapi.DatabaseTrustedSourceBody{Cidr: s.CIDR.ValueString()}
		if v := s.Note.ValueString(); v != "" {
			e.Note = &v
		}
		list = append(list, e)
	}
	out, err := r.client.API.DatabaseTrustedSourcesSetWithResponse(ctx, id,
		fbapi.DatabaseTrustedSourcesInputBody{Sources: &list})
	if err != nil {
		apiError(diags, "Setting the database's trusted sources", err)
		return false
	}
	if out.StatusCode() >= 400 {
		apiError(diags, "Setting the database's trusted sources",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	return true
}

func (r *databaseResource) refresh(ctx context.Context, id string, m *databaseModel, diags *diagSink) bool {
	out, err := r.client.API.DatabaseGetWithResponse(ctx, id)
	if err != nil {
		apiError(diags, "Re-reading the database", err)
		return false
	}
	if out.JSON200 == nil {
		apiError(diags, "Re-reading the database",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	applyDatabase(ctx, m, &out.JSON200.Database, out.JSON200, diags)
	return true
}

// applyDatabase fills the model. `detail` is the detail endpoint's body when
// there is one; a create response carries the instance alone, so the trusted
// source list is left as configured rather than blanked.
func applyDatabase(ctx context.Context, m *databaseModel, b *fbapi.DatabaseBody, detail *fbapi.DatabaseGetOutputBody, diags *diag.Diagnostics) {
	m.ID = types.StringValue(b.Id)
	m.Code = types.StringValue(b.Code)
	m.Name = types.StringValue(b.Name)
	m.Engine = types.StringValue(string(b.Engine))
	m.EngineVersion = types.StringValue(b.EngineVersion)
	m.State = types.StringValue(string(b.State))
	m.PublicAccess = types.BoolValue(b.PublicAccess)
	m.PendingApply = types.BoolValue(b.PendingApply)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.IP = optString(b.Ip)
	// The create's 202 body can omit these; every later read carries them.
	m.Plan = preferAPI(m.Plan, b.PlanSlug)
	m.Region = preferAPI(m.Region, b.RegionSlug)
	m.NetworkID = preferAPI(m.NetworkID, b.NetworkId)
	m.ProjectID = optString(b.ProjectId)
	applyTags(ctx, &m.Tags, b.Tags, diags)

	if detail == nil {
		return
	}
	var sources []dbTrustedSourceModel
	if detail.TrustedSources != nil {
		for _, s := range *detail.TrustedSources {
			sources = append(sources, dbTrustedSourceModel{
				CIDR: types.StringValue(s.Cidr),
				Note: optString(s.Note),
			})
		}
	}
	m.TrustedSources = sources
}

func dbSourcesEqual(a, b []dbTrustedSourceModel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].CIDR.Equal(b[i].CIDR) || !a[i].Note.Equal(b[i].Note) {
			return false
		}
	}
	return true
}
