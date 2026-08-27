package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
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
	_ resource.Resource                = (*serverResource)(nil)
	_ resource.ResourceWithConfigure   = (*serverResource)(nil)
	_ resource.ResourceWithImportState = (*serverResource)(nil)
)

func NewServerResource() resource.Resource { return &serverResource{} }

type serverResource struct{ resourceConfigure }

type serverModel struct {
	ID             types.String `tfsdk:"id"`
	Code           types.String `tfsdk:"code"`
	Name           types.String `tfsdk:"name"`
	Plan           types.String `tfsdk:"plan"`
	Image          types.String `tfsdk:"image"`
	Region         types.String `tfsdk:"region"`
	ProjectID      types.String `tfsdk:"project_id"`
	Tags           types.Set    `tfsdk:"tags"`
	NetworkID      types.String `tfsdk:"network_id"`
	FirewallID     types.String `tfsdk:"firewall_id"`
	SSHKeyIDs      types.List   `tfsdk:"ssh_key_ids"`
	UserData       types.String `tfsdk:"user_data"`
	WaitForRunning types.Bool   `tfsdk:"wait_for_running"`

	State     types.String `tfsdk:"state"`
	IPv4      types.String `tfsdk:"ipv4_address"`
	PrivateIP types.String `tfsdk:"private_ip"`
	CreatedAt types.String `tfsdk:"created_at"`
}

func (r *serverResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_server"
}

func (r *serverResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A virtual server.\n\n" +
			"A month is charged upfront when the server is created, and the unused part is " +
			"refunded when it is destroyed early. That makes `terraform apply` a purchase and " +
			"`terraform destroy` a partial refund, which is worth knowing before running either " +
			"in a loop.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The server's UUID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"code": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The short public number the panel shows and support asks for. " +
					"Stable for the server's whole life and never reused, even after deletion.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Hostname format: lowercase letters, digits and hyphens. " +
					"Renaming is applied in place and does not rebuild the machine.",
			},
			"plan": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Plan slug, e.g. `s1`. Use the `firstboot_plans` data source to " +
					"see what a region sells.\n\n" +
					"**Changing this is an UPGRADE ONLY and it restarts the server.** The API " +
					"refuses a downgrade, so a plan change to something smaller fails at apply " +
					"rather than at plan. Disk only ever grows.",
			},
			"image": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Image slug, e.g. `ubuntu-24-04`.",
				PlanModifiers: []planmodifier.String{
					// Changing the image means rebuilding the machine, which
					// destroys its disk. Terraform's own replace is the honest
					// way to express that: the operator sees "must be replaced"
					// in the plan instead of discovering it afterwards.
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Region slug, e.g. `ist`. Defaults to the platform's first " +
					"active region.\n\n" +
					"**There is no live migration between regions.** A region change is a new " +
					"machine with a new address, which is exactly what replacement means here.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"tags":       tagsAttribute("server"),
			"project_id": projectAttribute("server"),
			"network_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional private network to join at creation, which assigns a private address.\n\n" +
					"Attaching or detaching a network after creation needs a restart the customer " +
					"schedules themselves, so this provider treats a change as a replacement " +
					"rather than rebooting a running machine behind the operator's back.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"firewall_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional firewall attached from birth, so its rules are enforced " +
					"before the machine's first boot.\n\n" +
					"To attach or detach a firewall on a running server, use the firewall's own " +
					"membership instead; changing it here replaces the server.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"ssh_key_ids": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "SSH keys to inject at first boot. With at least one key the root " +
					"password is never set and root logs in by key only; with none, a generated " +
					"password is mailed to the account.\n\n" +
					"**Write-only.** The API does not return the keys a server was built with, so " +
					"this value is never refreshed and drift in it cannot be detected. A change " +
					"replaces the server, because keys are injected at first boot and adding one " +
					"later means rebuilding.",
				PlanModifiers: []planmodifier.List{listRequiresReplace{}},
			},
			"user_data": schema.StringAttribute{
				Optional:  true,
				Sensitive: false,
				MarkdownDescription: "A cloud-init document applied verbatim on first boot " +
					"(`#cloud-config`, a shell script, `#include` or MIME multipart). Up to 64 KiB, " +
					"OS images only.\n\n" +
					"**Readable from inside the guest by any process, so it must never carry " +
					"secrets.** It is marked non-sensitive here deliberately: marking it sensitive " +
					"would hide it from plan output while doing nothing about the fact that " +
					"anyone on the machine can read it, which is the misleading half.\n\n" +
					"**Write-only**, like `ssh_key_ids`: the API does not return it, and it only " +
					"ever runs once, at first boot.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"wait_for_running": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the server to finish provisioning. " +
					"Default `true`, and worth leaving on: with it off, `ipv4_address` is whatever " +
					"the API knew at create time and anything downstream (a DNS record, a " +
					"provisioner) runs against a machine that may not be up.",
			},

			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The server's lifecycle state. `running` and `stopped` are settled; " +
					"an `error_*` value means provisioning failed.",
			},
			"ipv4_address": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The public IPv4 address, assigned during provisioning.",
			},
			"private_ip": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The private address, when the server joined a private network.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *serverResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan serverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := fbapi.CreateInputBody{
		Name: plan.Name.ValueString(),
		Plan: plan.Plan.ValueString(),
	}
	if v := plan.Image.ValueString(); v != "" {
		body.Image = &v
	}
	if v := plan.Region.ValueString(); v != "" && !plan.Region.IsUnknown() {
		body.Region = &v
	}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	body.Tags = tagsFromPlan(ctx, plan.Tags, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if v := plan.NetworkID.ValueString(); v != "" {
		body.NetworkId = &v
	}
	if v := plan.FirewallID.ValueString(); v != "" {
		body.FirewallId = &v
	}
	if v := plan.UserData.ValueString(); v != "" {
		body.UserData = &v
	}
	if !plan.SSHKeyIDs.IsNull() && !plan.SSHKeyIDs.IsUnknown() {
		var ids []string
		resp.Diagnostics.Append(plan.SSHKeyIDs.ElementsAs(ctx, &ids, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.SshKeyIds = &ids
	}

	// The SDK sets an Idempotency-Key and reuses it across its own retries, so
	// a create whose response is lost cannot open a second machine. That is the
	// single most important thing this provider gets for free.
	out, err := r.client.API.ServerCreateWithResponse(ctx, &fbapi.ServerCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the server", err)
		return
	}
	if out.JSON202 == nil {
		apiError(&resp.Diagnostics, "Creating the server",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}

	srv := &out.JSON202.Server
	applyServer(ctx, &plan, srv, &resp.Diagnostics)
	// State is written BEFORE the wait. If the wait then fails or the operator
	// interrupts it, Terraform still knows the server exists -- without this,
	// an interrupted apply leaves a running, billing machine that no state file
	// names and that the next apply would duplicate.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !plan.WaitForRunning.ValueBool() {
		return
	}
	settled, err := r.client.WaitForServer(ctx, srv.Id)
	if err != nil {
		waitError(&resp.Diagnostics, "Waiting for the server", err)
		return
	}
	applyServer(ctx, &plan, settled, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serverResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serverModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.ServerGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the server", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the server",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyServer(ctx, &state, out.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *serverResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state serverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	// Three independent endpoints, applied in a fixed order: rename first
	// because it is free and cannot fail for capacity reasons, then the project
	// move, then the resize, which restarts the machine. A resize that fails
	// therefore leaves the cheap changes already applied rather than losing them.
	if !plan.Name.Equal(state.Name) {
		out, err := r.client.API.ServerRenameWithResponse(ctx, id,
			fbapi.RenameInputBody{Name: plan.Name.ValueString()})
		if err != nil {
			apiError(&resp.Diagnostics, "Renaming the server", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Renaming the server",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
	}

	if !applyGrouping(ctx, &resp.Diagnostics, groupingUpdate{
		Noun: "server", PlanTags: plan.Tags, StateTags: state.Tags,
		PlanProject: plan.ProjectID, StateProject: state.ProjectID,
		SetTags: func(ctx context.Context, b fbapi.TagsBody) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.ServerTagsSetWithResponse(ctx, id, b)
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
		SetProject: func(ctx context.Context, pid *string) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.ServerProjectSetWithResponse(ctx, id,
				fbapi.ServerProjectSetJSONRequestBody{ProjectId: pid})
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
	}) {
		return
	}

	if !plan.Plan.Equal(state.Plan) {
		out, err := r.client.API.ServerResizeWithResponse(ctx, id,
			fbapi.ResizeInputBody{Plan: plan.Plan.ValueString()})
		if err != nil {
			apiError(&resp.Diagnostics, "Resizing the server", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Resizing the server",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
		// A resize restarts the machine, so the read below would otherwise
		// catch it mid-transition and write `resizing` into state as if it
		// were the settled answer.
		if plan.WaitForRunning.ValueBool() {
			if _, err := r.client.WaitForServer(ctx, id); err != nil {
				waitError(&resp.Diagnostics, "Waiting for the resize", err)
				return
			}
		}
	}

	out, err := r.client.API.ServerGetWithResponse(ctx, id)
	if err != nil {
		apiError(&resp.Diagnostics, "Re-reading the server", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Re-reading the server",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyServer(ctx, &plan, out.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serverResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state serverModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.ServerDeleteWithResponse(ctx, state.ID.ValueString(), &fbapi.ServerDeleteParams{})
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the server", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the server",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	// The delete is asynchronous and the API answers 202. Terraform's contract
	// is that Delete returning means the resource is gone from its point of
	// view, and the unused month is refunded whether or not this process waits,
	// so there is deliberately no wait here: blocking an apply on a teardown
	// buys nothing.
}

// ImportState takes the server's UUID or its short code -- the API's detail
// endpoint resolves both, and the code is what a person has in front of them
// when they go looking. The write-only attributes cannot come back, which is
// what the warning is for.
func (r *serverResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.AddWarning("Write-only attributes cannot be imported",
		"`ssh_key_ids` and `user_data` are not returned by the API, so an imported server has "+
			"neither. If the configuration sets them, the next plan will want to REPLACE the "+
			"server, which destroys it.\n\n"+
			"Either leave both out of the configuration for imported servers, or add an "+
			"`ignore_changes` lifecycle block for them after checking the plan.")
}

func applyServer(ctx context.Context, m *serverModel, b *fbapi.ServerBody, diags *diag.Diagnostics) {
	m.ID = types.StringValue(b.Id)
	m.Code = types.StringValue(b.Code)
	m.Name = types.StringValue(b.Name)
	m.State = types.StringValue(string(b.State))
	m.Plan = types.StringValue(b.Plan.Slug)
	m.Image = types.StringValue(b.Image.Slug)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.IPv4 = types.StringValue(derefString(b.Ip))
	m.PrivateIP = types.StringValue(derefString(b.PrivateIp))
	if b.Region != nil {
		m.Region = types.StringValue(b.Region.Slug)
	}
	// project_id is refreshed because the API returns it and a move made in the
	// panel is real drift. ssh_key_ids and user_data are NOT: the API does not
	// return them, and writing a null over the configured value would make
	// every plan want to replace the machine.
	if b.ProjectId != nil && *b.ProjectId != "" {
		m.ProjectID = types.StringValue(*b.ProjectId)
	} else {
		m.ProjectID = types.StringNull()
	}
	// Tags are refreshed for the same reason project_id is: the API returns
	// them, so a tag added in the panel is real drift.
	applyTags(ctx, &m.Tags, b.Tags, diags)
}

// listRequiresReplace is RequiresReplace for a list attribute. The framework
// ships stringplanmodifier.RequiresReplace but the list equivalent lives in
// listplanmodifier; this exists so the intent is stated in one place next to
// the attribute it guards.
type listRequiresReplace struct{}

func (listRequiresReplace) Description(context.Context) string {
	return "Changing this replaces the server: SSH keys are injected at first boot."
}
func (listRequiresReplace) MarkdownDescription(ctx context.Context) string {
	return listRequiresReplace{}.Description(ctx)
}
func (listRequiresReplace) PlanModifyList(_ context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	// Nothing to replace on create or destroy.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if !req.PlanValue.Equal(req.StateValue) {
		resp.RequiresReplace = true
	}
}

var _ planmodifier.List = listRequiresReplace{}
