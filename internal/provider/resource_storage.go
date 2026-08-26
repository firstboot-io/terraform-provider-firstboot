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

	"github.com/firstboot-io/firstboot-go/fbapi"
)

// ============================== volume ==============================

var (
	_ resource.Resource                = (*volumeResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeResource)(nil)
	_ resource.ResourceWithImportState = (*volumeResource)(nil)
)

func NewVolumeResource() resource.Resource { return &volumeResource{} }

type volumeResource struct{ resourceConfigure }

type volumeModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	SizeGB    types.Int64  `tfsdk:"size_gb"`
	ProjectID types.String `tfsdk:"project_id"`
	ServerID  types.String `tfsdk:"server_id"`
	FSType    types.String `tfsdk:"fs_type"`
	Automount types.Bool   `tfsdk:"automount"`
	WaitFor   types.Bool   `tfsdk:"wait_for_ready"`

	State      types.String `tfsdk:"state"`
	MountState types.String `tfsdk:"mount_state"`
	MountPath  types.String `tfsdk:"mount_path"`
	CreatedAt  types.String `tfsdk:"created_at"`
}

func (r *volumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

func (r *volumeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A block storage volume.\n\n" +
			"Billed per GB-hour at its PROVISIONED size from creation to deletion, whether or " +
			"not it is attached. A detached volume still costs money.\n\n" +
			"**Volumes are excluded from server backups.** Whatever lives here needs its own " +
			"backup arrangement.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The volume's name. The API has no rename, so changing it " +
					"replaces the volume, which DESTROYS its data.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"size_gb": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Size in GB. **Grow only.** A smaller value is refused by the API " +
					"at apply time rather than at plan time, and growing charges the difference. " +
					"Read `firstboot_volume_limits` rather than assuming a band: the minimum and " +
					"maximum are operator policy with per-account overrides.",
			},
			"project_id": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"server_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Server to attach to. Changing this detaches from one server and " +
					"attaches to the other, in place; it does not replace the volume. " +
					"At most five volumes per server.",
			},
			"fs_type": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Filesystem to create at BIRTH, e.g. `ext4` or `xfs`. Omit for a " +
					"raw block device.\n\n" +
					"**Accepted only at creation and never again.** A volume is formatted once, " +
					"when the disk is provably empty; afterwards nothing can honestly answer " +
					"\"does this hold data\", so nothing is allowed to try. Changing it replaces " +
					"the volume and destroys what is on it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"automount": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether the volume is mounted inside the guest on attach, via an " +
					"fstab line keyed by UUID and carrying `nofail`.\n\n" +
					"Defaults to `false` for a reason: a running server's fstab was written by its " +
					"owner, and mounting over it on the next attach is a surprise rather than a " +
					"convenience.",
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the volume to finish provisioning.",
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`available` or `attached` are settled; `error` is a failure.",
			},
			"mount_state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Whether the in-guest mount succeeded. A failure here does NOT " +
					"fail the attachment: the disk really is attached and the manual instructions " +
					"still work.",
			},
			"mount_path": schema.StringAttribute{Computed: true},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *volumeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := fbapi.VolumeCreateInputBody{
		Name:      plan.Name.ValueString(),
		SizeGb:    plan.SizeGB.ValueInt64(),
		Automount: ptr(plan.Automount.ValueBool()),
	}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	if v := plan.ServerID.ValueString(); v != "" {
		body.ServerId = &v
	}
	if v := plan.FSType.ValueString(); v != "" {
		t := fbapi.VolumeCreateInputBodyFsType(v)
		body.FsType = &t
	}

	out, err := r.client.API.VolumeCreateWithResponse(ctx, &fbapi.VolumeCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the volume", err)
		return
	}
	if out.JSON202 == nil {
		apiError(&resp.Diagnostics, "Creating the volume",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyVolume(&plan, out.JSON202)
	// State before the wait, for the same reason as the server: an interrupted
	// apply must not leave a billing volume that no state file names.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() || !plan.WaitFor.ValueBool() {
		return
	}
	settled, err := r.client.WaitForVolume(ctx, uuidOf(out.JSON202.Id))
	if err != nil {
		waitError(&resp.Diagnostics, "Waiting for the volume", err)
		return
	}
	applyVolume(&plan, settled)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.VolumeGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the volume", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the volume",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyVolume(&state, out.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *volumeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	// Resize before the attachment change: growing a volume that is about to
	// move is the same operation either way, and doing it while it is still on
	// the old server keeps the two failures separable.
	if !plan.SizeGB.Equal(state.SizeGB) {
		out, err := r.client.API.VolumeResizeWithResponse(ctx, id,
			fbapi.VolumeResizeInputBody{SizeGb: plan.SizeGB.ValueInt64()})
		if err != nil {
			apiError(&resp.Diagnostics, "Resizing the volume", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Resizing the volume",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
	}

	if !plan.ServerID.Equal(state.ServerID) {
		// Detach first whenever there is something to detach: the API takes at
		// most five volumes per server and a move that attached before
		// detaching would count against both.
		if state.ServerID.ValueString() != "" {
			out, err := r.client.API.VolumeDetachWithResponse(ctx, id)
			if err != nil {
				apiError(&resp.Diagnostics, "Detaching the volume", err)
				return
			}
			if out.StatusCode() >= 400 {
				apiError(&resp.Diagnostics, "Detaching the volume",
					problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
				return
			}
		}
		if v := plan.ServerID.ValueString(); v != "" {
			out, err := r.client.API.VolumeAttachWithResponse(ctx, id,
				fbapi.VolumeAttachInputBody{ServerId: v, Automount: ptr(plan.Automount.ValueBool())})
			if err != nil {
				apiError(&resp.Diagnostics, "Attaching the volume", err)
				return
			}
			if out.StatusCode() >= 400 {
				apiError(&resp.Diagnostics, "Attaching the volume",
					problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
				return
			}
		}
	}

	if plan.WaitFor.ValueBool() {
		if _, err := r.client.WaitForVolume(ctx, uuidOf(id)); err != nil {
			waitError(&resp.Diagnostics, "Waiting for the volume", err)
			return
		}
	}
	out, err := r.client.API.VolumeGetWithResponse(ctx, id)
	// The transport error is checked on its own, before anything reads a status
	// off the response: a transport error returns a NIL response, and
	// out.StatusCode() on nil panics rather than answering zero. A panic here
	// crashes the provider plugin and Terraform reports it as "the plugin
	// exited unexpectedly", which says nothing about the network blip that
	// caused it.
	if err != nil {
		apiError(&resp.Diagnostics, "Re-reading the volume", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Re-reading the volume",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyVolume(&plan, out.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.VolumeDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the volume", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the volume",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *volumeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func applyVolume(m *volumeModel, b *fbapi.VolumeBody) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.SizeGB = types.Int64Value(b.SizeGb)
	m.State = types.StringValue(string(b.State))
	m.MountState = types.StringValue(b.MountState)
	m.Automount = types.BoolValue(b.Automount)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.MountPath = optString(b.MountPath)
	m.FSType = optString(b.FsType)
	m.ServerID = optString(b.ServerId)
}

// ============================== network ==============================

var (
	_ resource.Resource                = (*networkResource)(nil)
	_ resource.ResourceWithConfigure   = (*networkResource)(nil)
	_ resource.ResourceWithImportState = (*networkResource)(nil)
)

func NewNetworkResource() resource.Resource { return &networkResource{} }

type networkResource struct{ resourceConfigure }

type networkModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	CIDR      types.String `tfsdk:"cidr"`
	ProjectID types.String `tfsdk:"project_id"`
	State     types.String `tfsdk:"state"`
	CreatedAt types.String `tfsdk:"created_at"`
}

func (r *networkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_network"
}

func (r *networkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A private network. Servers that join it reach each other on private " +
			"addresses.\n\n" +
			"**The API has no update for a network at all**, so every attribute here requires " +
			"replacement. That is a real constraint rather than a provider limitation: a CIDR " +
			"change would strand every member's address.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cidr": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The private range, e.g. `10.10.0.0/24`. Servers already " +
					"referencing the network wait for it: provisioning does not race it.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"project_id": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`active` or `error` are settled; `creating` and `deleting` are in flight.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *networkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := fbapi.NetworkCreateInputBody{Name: plan.Name.ValueString(), Cidr: plan.CIDR.ValueString()}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	out, err := r.client.API.NetworkCreateWithResponse(ctx, &fbapi.NetworkCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the network", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the network",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyNetwork(&plan, out.JSON201)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.NetworkGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the network", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the network",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyNetwork(&state, &out.JSON200.Network)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *networkResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Private networks cannot be updated",
		"The API has no update endpoint for a network, so every attribute requires replacement "+
			"and reaching Update is a bug in the provider. Please report it.")
}

func (r *networkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.NetworkDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the network", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the network",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *networkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func applyNetwork(m *networkModel, b *fbapi.NetworkBody) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.CIDR = types.StringValue(b.Cidr)
	m.State = types.StringValue(string(b.State))
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.ProjectID = optString(b.ProjectId)
}
