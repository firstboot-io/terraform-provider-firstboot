package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/go-sdk/fbapi"
)

var (
	_ resource.Resource                = (*projectResource)(nil)
	_ resource.ResourceWithConfigure   = (*projectResource)(nil)
	_ resource.ResourceWithImportState = (*projectResource)(nil)
)

func NewProjectResource() resource.Resource { return &projectResource{} }

type projectResource struct{ resourceConfigure }

type projectModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	CreatedAt types.String `tfsdk:"created_at"`
}

func (r *projectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (r *projectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A project groups servers, volumes and other resources for organisation " +
			"and reporting. It is a label, not a boundary: a project does not isolate anything " +
			"and does not affect billing.\n\n" +
			"Deleting a project does NOT delete what was in it. The resources are simply no " +
			"longer grouped, which is why destroying a project is safe and why it is not a " +
			"substitute for destroying the servers.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The project's name. Renaming is applied in place, not by replacement.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *projectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan projectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.ProjectCreateWithResponse(ctx, &fbapi.ProjectCreateParams{},
		fbapi.ProjectCreateInputBody{Name: plan.Name.ValueString()})
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the project", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the project",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyProject(&plan, out.JSON201)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *projectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state projectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.ProjectGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the project", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the project",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyProject(&state, out.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *projectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state projectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The id comes from STATE, never from the plan: a computed attribute is
	// unknown in the plan for a resource being changed, and reading it there
	// sends an empty path parameter.
	out, err := r.client.API.ProjectUpdateWithResponse(ctx, state.ID.ValueString(),
		fbapi.ProjectUpdateInputBody{Name: plan.Name.ValueString()})
	if err != nil {
		apiError(&resp.Diagnostics, "Renaming the project", err)
		return
	}
	if out.StatusCode() >= 400 {
		apiError(&resp.Diagnostics, "Renaming the project",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	// The rename answers with no body, so the rest of the state carries over
	// from what was already there rather than being re-read for one field.
	plan.ID, plan.CreatedAt = state.ID, state.CreatedAt
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *projectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state projectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.ProjectDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the project", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the project",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *projectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func applyProject(m *projectModel, b *fbapi.ProjectBody) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
}
