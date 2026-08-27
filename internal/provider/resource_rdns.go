package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/go-sdk/fbapi"
)

// Reverse DNS, and why it is its own resource.
//
// rDNS is not a resource in the API: it is one editable FIELD on an address,
// managed with a PUT and a DELETE. Two shapes were possible and the third was
// the one chosen.
//
//   - A nested block on `firstboot_server` reads well until a floating IP is
//     involved. The PTR belongs to the ADDRESS, and a floating IP outlives the
//     server it currently points at; modelling it inside the server would make
//     moving the address silently rewrite somebody's PTR.
//   - A resource keyed on the ip-address id alone cannot be READ. There is no
//     GET for one address's rDNS -- the only way to see it is the list under
//     `/v1/servers/{id}/rdns` -- so a refresh needs the server too.
//
// So the identity is (server, address): the server is how the entry is found,
// and the address is which of that server's addresses the record is for. The
// resource's own id is the ip-address id, because that is what the set and
// clear calls take.
var (
	_ resource.Resource                = (*rdnsResource)(nil)
	_ resource.ResourceWithConfigure   = (*rdnsResource)(nil)
	_ resource.ResourceWithImportState = (*rdnsResource)(nil)
)

func NewRDNSResource() resource.Resource { return &rdnsResource{} }

type rdnsResource struct{ resourceConfigure }

type rdnsModel struct {
	ID       types.String `tfsdk:"id"`
	ServerID types.String `tfsdk:"server_id"`
	Address  types.String `tfsdk:"address"`
	Hostname types.String `tfsdk:"hostname"`

	IsFloating  types.Bool   `tfsdk:"is_floating"`
	ReverseZone types.String `tfsdk:"reverse_zone"`
}

func (r *rdnsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_rdns"
}

func (r *rdnsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The PTR record for one of a server's public addresses.\n\n" +
			"**The hostname must ALREADY resolve to this address before the PTR is accepted.** " +
			"The platform confirms the forward record itself and refuses otherwise, which is " +
			"what stops anyone claiming anyone else's name. In Terraform terms that means the " +
			"`firstboot_dns_record` has to exist first, and has to have propagated -- an apply " +
			"that creates both in one run can fail here and succeed on the next one.\n\n" +
			"Destroying this resource withdraws the PTR; it does not touch the address.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The IP address's id, which is what the API's set and clear calls " +
					"take. It is not the server's id and not the floating IP's.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"server_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The server the address belongs to, or the server a floating IP is " +
					"attached to. It is how the entry is found: there is no endpoint that reads " +
					"one address's rDNS on its own.\n\n" +
					"A change points the record at a different machine's address, which is a " +
					"different record, so it replaces this one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"address": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Which of the server's addresses to publish a PTR for. Omit it for " +
					"the server's own fixed address; give a floating IP's address to set the " +
					"record for that instead.\n\n" +
					"A change is a different address and therefore a different record.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"hostname": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The fully qualified name to publish. Changed in place.",
			},

			"is_floating": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "True when the address is a floating IP attached to this server " +
					"rather than the server's own.",
			},
			"reverse_zone": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The `in-addr.arpa` zone that carries this PTR. Empty means rDNS is " +
					"not available for this address at all, which is a delegation the platform " +
					"does not hold rather than something a configuration can fix.",
			},
		},
	}
}

func (r *rdnsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan rdnsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entry, ok := r.findEntry(ctx, plan.ServerID.ValueString(), plan.Address, &resp.Diagnostics)
	if !ok {
		return
	}
	if !r.set(ctx, entry.Id, plan.Hostname.ValueString(), &resp.Diagnostics) {
		return
	}
	applyRDNS(&plan, entry)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read walks the server's entries. There is no per-address GET, so the server's
// list is the only way to refresh one -- which is also why an address that no
// longer appears there is treated as gone rather than as an error: a released
// floating IP takes its entry with it.
func (r *rdnsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state rdnsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entries, ok := r.listEntries(ctx, state.ServerID.ValueString(), &resp.Diagnostics)
	if !ok {
		if resp.Diagnostics.HasError() {
			return
		}
		// The server itself is gone, so the record is too.
		resp.State.RemoveResource(ctx)
		return
	}
	id := state.ID.ValueString()
	for i := range entries {
		if entries[i].Id != id {
			continue
		}
		// A hostname the API no longer carries means the PTR was withdrawn
		// outside Terraform. That is drift in the record rather than the
		// disappearance of the address, so the resource stays and the empty
		// value is what the next plan reconciles.
		applyRDNS(&state, &entries[i])
		state.Hostname = optString(entries[i].Hostname)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	resp.State.RemoveResource(ctx)
}

func (r *rdnsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state rdnsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.set(ctx, state.ID.ValueString(), plan.Hostname.ValueString(), &resp.Diagnostics) {
		return
	}
	plan.ID, plan.Address = state.ID, state.Address
	plan.IsFloating, plan.ReverseZone = state.IsFloating, state.ReverseZone
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *rdnsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state rdnsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.RdnsClearWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Clearing the reverse DNS record", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Clearing the reverse DNS record",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

// ImportState takes `server_id/address`, because neither half is enough: the
// address says which record, and the server is the only way to read it.
func (r *rdnsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Unexpected import id",
			fmt.Sprintf("Expected `server_id/address`, got %q.\n\n"+
				"An address's rDNS has no endpoint of its own -- it is read through the server "+
				"that carries the address -- so the server's id has to come with it.", req.ID))
		return
	}
	entry, ok := r.findEntry(ctx, parts[0], types.StringValue(parts[1]), &resp.Diagnostics)
	if !ok {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("server_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), entry.Id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("address"), entry.Address)...)
}

func (r *rdnsResource) set(ctx context.Context, ipAddressID, hostname string, diags *diagSink) bool {
	out, err := r.client.API.RdnsSetWithResponse(ctx, ipAddressID,
		fbapi.RdnsSetInputBody{Hostname: hostname})
	if err != nil {
		apiError(diags, "Setting the reverse DNS record", err)
		return false
	}
	if out.JSON200 == nil {
		apiError(diags, "Setting the reverse DNS record",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	return true
}

// listEntries returns the server's editable addresses. The second return value
// is false when the server is gone, which the caller distinguishes from an error
// by checking the diagnostics.
func (r *rdnsResource) listEntries(ctx context.Context, serverID string, diags *diagSink) ([]fbapi.RdnsEntryBody, bool) {
	out, err := r.client.API.ServerRDNSListWithResponse(ctx, serverID)
	if err != nil {
		apiError(diags, "Reading the server's reverse DNS entries", err)
		return nil, false
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			return nil, false
		}
		apiError(diags, "Reading the server's reverse DNS entries",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return nil, false
	}
	if out.JSON200.Entries == nil {
		return nil, true
	}
	return *out.JSON200.Entries, true
}

// findEntry resolves which of the server's addresses the configuration means.
// An unset address means the server's own fixed one, which is the entry that is
// not a floating IP.
func (r *rdnsResource) findEntry(ctx context.Context, serverID string, address types.String, diags *diagSink) (*fbapi.RdnsEntryBody, bool) {
	entries, ok := r.listEntries(ctx, serverID, diags)
	if !ok {
		if !diags.HasError() {
			diags.AddError("The server no longer exists",
				fmt.Sprintf("No server with id %q, so it has no addresses to publish a PTR for.", serverID))
		}
		return nil, false
	}

	want := address.ValueString()
	if want == "" || address.IsUnknown() {
		for i := range entries {
			if !entries[i].IsFloating {
				return &entries[i], true
			}
		}
		diags.AddError("The server has no fixed public address",
			"`address` was not set, so the provider looked for the server's own public address "+
				"and found none. A server that only carries floating IPs has to name which one "+
				"this record is for.")
		return nil, false
	}
	for i := range entries {
		if entries[i].Address == want {
			return &entries[i], true
		}
	}

	var have []string
	for i := range entries {
		have = append(have, entries[i].Address)
	}
	detail := fmt.Sprintf("The server does not carry the address %q.", want)
	if len(have) > 0 {
		detail += "\n\nIt carries: " + strings.Join(have, ", ")
	} else {
		detail += "\n\nIt carries no addresses that accept a PTR."
	}
	detail += "\n\nA floating IP only appears here while it is ATTACHED to this server, so an " +
		"apply that creates the attachment in the same run has to depend on it."
	diags.AddError("No such address on that server", detail)
	return nil, false
}

func applyRDNS(m *rdnsModel, e *fbapi.RdnsEntryBody) {
	m.ID = types.StringValue(e.Id)
	m.Address = types.StringValue(e.Address)
	m.IsFloating = types.BoolValue(e.IsFloating)
	m.ReverseZone = optString(e.ReverseZone)
}
