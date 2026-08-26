package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/firstboot-go/fbapi"
)

var (
	_ resource.Resource                = (*sshKeyResource)(nil)
	_ resource.ResourceWithConfigure   = (*sshKeyResource)(nil)
	_ resource.ResourceWithImportState = (*sshKeyResource)(nil)
)

func NewSSHKeyResource() resource.Resource { return &sshKeyResource{} }

type sshKeyResource struct{ resourceConfigure }

type sshKeyModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	PublicKey   types.String `tfsdk:"public_key"`
	Fingerprint types.String `tfsdk:"fingerprint"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

func (r *sshKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ssh_key"
}

func (r *sshKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An SSH public key that can be injected into a server at creation.\n\n" +
			"Keys belong to the ORGANIZATION, not to the member who added them: another member " +
			"can select this key when creating a server, and deleting it here removes it for " +
			"everyone. Removing a key does not touch servers already built with it, which keep " +
			"their own snapshot of the keys they were born with.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The key's UUID.",
				PlanModifiers: []planmodifier.String{
					// The server assigns it and it never changes, so telling
					// Terraform that keeps every subsequent plan from showing
					// it as "(known after apply)".
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "A label, shown in the panel's key picker.",
				PlanModifiers: []planmodifier.String{
					// The API has no update for a key: it is created and
					// deleted, nothing in between. A rename is therefore a
					// replacement, and saying so in the schema is what makes
					// the plan honest instead of applying and silently
					// changing nothing.
					stringplanmodifier.RequiresReplace(),
				},
			},
			"public_key": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The public key in `authorized_keys` form, e.g. " +
					"`ssh-ed25519 AAAA… name@host`.\n\n" +
					"The API normalises what it stores (it parses the key and re-serialises it), " +
					"so this value stays exactly as written in the configuration and is never " +
					"read back. A change replaces the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"fingerprint": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "SHA256 fingerprint, computed by the API from the parsed key. " +
					"This is the value to compare against `ssh-keygen -lf`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *sshKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sshKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.API.SshKeyCreateWithResponse(ctx, &fbapi.SshKeyCreateParams{},
		fbapi.KeyCreateInputBody{
			Name:      plan.Name.ValueString(),
			PublicKey: plan.PublicKey.ValueString(),
		})
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the SSH key", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the SSH key",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	// public_key is NOT read back: the API stores a normalised form and the
	// body has never carried it. Keeping the configured value is what stops
	// every plan reporting a diff on a key that is byte-identical in meaning.
	applySSHKey(&plan, out.JSON201)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sshKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sshKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.API.SshKeyGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the SSH key", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			// Deleted outside Terraform. Removing it from state is the contract;
			// erroring would leave a configuration that can never apply again.
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the SSH key",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applySSHKey(&state, out.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update exists only because the framework requires it. Every attribute is
// RequiresReplace, so this can be reached only by a bug in the schema above --
// and answering with a silent no-op would leave state claiming a change the API
// never made.
func (r *sshKeyResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("SSH keys cannot be updated",
		"Every attribute of firstboot_ssh_key requires replacement, so reaching Update is a "+
			"bug in the provider. Please report it.")
}

func (r *sshKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sshKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.API.SshKeyDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the SSH key", err)
		return
	}
	// A key already gone is a successful delete. Erroring here is how a
	// `terraform destroy` gets stuck on something that no longer exists.
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the SSH key",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

// ImportState works because the API grew `GET /v1/ssh-keys/{id}` on 2026-08-24.
// Before that only a list existed and this resource could not be imported at
// all: `terraform import` is handed an id and nothing else.
func (r *sshKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	// public_key cannot be recovered -- the API does not return it -- so an
	// imported key shows a diff on the next plan unless the configuration
	// happens to match what was uploaded. Saying so at import time is better
	// than letting the operator discover it as a mysterious replacement.
	resp.Diagnostics.AddWarning("public_key cannot be imported",
		"The API does not return the stored public key, so the imported resource has none. "+
			"The next plan will want to replace the key unless `public_key` in the "+
			"configuration matches what was uploaded. Compare the `fingerprint` attribute "+
			"against `ssh-keygen -lf your_key.pub` to confirm they are the same key.")
}

func applySSHKey(m *sshKeyModel, b *fbapi.SshKeyBody) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.Fingerprint = types.StringValue(b.Fingerprint)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
}

// timeFormat is RFC3339 with seconds. The API sends RFC3339; rendering it back
// in the same shape keeps a state file diffable against an API response.
const timeFormat = "2006-01-02T15:04:05Z07:00"
