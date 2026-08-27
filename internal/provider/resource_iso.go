package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/go-sdk/fbapi"
)

var (
	_ resource.Resource                = (*isoResource)(nil)
	_ resource.ResourceWithConfigure   = (*isoResource)(nil)
	_ resource.ResourceWithImportState = (*isoResource)(nil)
)

func NewISOResource() resource.Resource { return &isoResource{} }

type isoResource struct{ resourceConfigure }

type isoModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	URL      types.String `tfsdk:"url"`
	Checksum types.String `tfsdk:"checksum"`
	WaitFor  types.Bool   `tfsdk:"wait_for_ready"`

	SourceType   types.String `tfsdk:"source_type"`
	Status       types.String `tfsdk:"status"`
	Filename     types.String `tfsdk:"filename"`
	SizeBytes    types.Int64  `tfsdk:"size_bytes"`
	ErrorMessage types.String `tfsdk:"error_message"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func (r *isoResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_iso"
}

func (r *isoResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A custom ISO image, fetched from a URL you control and mountable on a " +
			"server.\n\n" +
			"**Nothing about it can be edited.** The API offers a create, a read and a delete, " +
			"so every argument here forces replacement -- which for an ISO means downloading it " +
			"again.\n\n" +
			"Mounting is a server ACTION rather than a property, so it is not modelled here: " +
			"mounting an ISO reboots the machine into it, which is a thing an operator does " +
			"deliberately rather than something a plan should decide.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The display name. There is no rename, so a change replaces the ISO.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"url": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Public `http(s)` URL to download from. It has to stay reachable " +
					"until the download finishes; the fetch runs on the platform's side, not in " +
					"the Terraform process.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"checksum": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional hex `md5`, `sha1`, `sha256` or `sha512` digest. The " +
					"download is verified against it and rejected on a mismatch.\n\n" +
					"Worth setting: without it, an ISO is whatever the URL served at the moment " +
					"the platform fetched it, and nothing afterwards can tell you what that was.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the download to finish. Leaving it on is " +
					"the difference between a plan that fails on a bad URL and one that succeeds " +
					"while the ISO ends up in `error`.",
			},

			"source_type": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Always `url` today. Direct upload is not available through the API, " +
					"which is why there is no file argument here.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`ready` and `error` are settled; `pending` and `downloading` are " +
					"in flight.",
			},
			"filename":   schema.StringAttribute{Computed: true},
			"size_bytes": schema.Int64Attribute{Computed: true},
			"error_message": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the download failed, when it did.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *isoResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan isoModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	url := plan.URL.ValueString()
	body := fbapi.IsoCreateInputBody{
		Name:       plan.Name.ValueString(),
		SourceType: fbapi.Url,
		Url:        &url,
	}
	if v := plan.Checksum.ValueString(); v != "" {
		body.Checksum = &v
	}

	out, err := r.client.API.IsoCreateWithResponse(ctx, &fbapi.IsoCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the ISO", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Creating the ISO",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	id := out.JSON200.Iso.Id
	applyISO(&plan, &out.JSON200.Iso)
	// State before the wait. An ISO occupies storage from the moment it is
	// created, so an interrupted apply must not leave one nothing names.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() || !plan.WaitFor.ValueBool() {
		return
	}
	settled, err := r.client.WaitForISO(ctx, id)
	if err != nil {
		waitError(&resp.Diagnostics, "Waiting for the ISO", err)
		return
	}
	applyISO(&plan, settled)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *isoResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state isoModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.IsoGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the ISO", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the ISO",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyISO(&state, &out.JSON200.Iso)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update cannot be reached: every configurable attribute requires replacement.
// It exists because the interface demands it, and it says so rather than
// silently doing nothing.
func (r *isoResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("An ISO cannot be updated",
		"The API has no update endpoint for an ISO, so every attribute requires replacement "+
			"and reaching Update is a bug in the provider. Please report it.")
}

func (r *isoResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state isoModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.IsoDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the ISO", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the ISO",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *isoResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func applyISO(m *isoModel, b *fbapi.IsoBody) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.SourceType = types.StringValue(b.SourceType)
	m.Status = types.StringValue(string(b.Status))
	m.Filename = types.StringValue(b.Filename)
	m.SizeBytes = types.Int64Value(b.SizeBytes)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.ErrorMessage = optString(b.ErrorMessage)
	// `url` is refreshed because the API returns it; `checksum` is too, and both
	// force replacement, so drift in either is a real answer rather than a
	// value the provider had to keep to itself.
	if b.Url != nil && *b.Url != "" {
		m.URL = types.StringValue(*b.Url)
	}
	m.Checksum = preferAPI(m.Checksum, b.Checksum)
}
