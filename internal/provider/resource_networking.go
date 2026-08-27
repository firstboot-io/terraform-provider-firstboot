package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/firstboot-go/fbapi"
)

// ============================== firewall ==============================

var (
	_ resource.Resource                = (*firewallResource)(nil)
	_ resource.ResourceWithConfigure   = (*firewallResource)(nil)
	_ resource.ResourceWithImportState = (*firewallResource)(nil)
)

func NewFirewallResource() resource.Resource { return &firewallResource{} }

type firewallResource struct{ resourceConfigure }

type firewallRuleModel struct {
	Direction types.String `tfsdk:"direction"`
	Protocol  types.String `tfsdk:"protocol"`
	Source    types.String `tfsdk:"source"`
	PortFrom  types.Int64  `tfsdk:"port_from"`
	PortTo    types.Int64  `tfsdk:"port_to"`
}

type firewallModel struct {
	ID        types.String        `tfsdk:"id"`
	Name      types.String        `tfsdk:"name"`
	Rules     []firewallRuleModel `tfsdk:"rule"`
	CreatedAt types.String        `tfsdk:"created_at"`
}

func (r *firewallResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall"
}

func (r *firewallResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A firewall: a named set of rules that can be attached to servers.\n\n" +
			"Attached to a server it enforces **deny-all inbound** (plus ICMP) until rules are " +
			"added, so an empty firewall is not a permissive one. Create it with its rules in " +
			"the same apply, or the machine is unreachable in between.\n\n" +
			"Attach it to a server through the server's `firewall_id`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{Required: true},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
		Blocks: map[string]schema.Block{
			// A BLOCK rather than one resource per rule, because the API
			// replaces the whole rule set in one call. Modelling a rule as its
			// own resource would mean N calls that each rewrite the same list,
			// and two of them applying concurrently would lose rules.
			"rule": schema.ListNestedBlock{
				MarkdownDescription: "One rule. The whole set is replaced on every change, which is " +
					"what the API offers and what keeps concurrent edits from losing rules.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"direction": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "`in` or `out`.",
						},
						"protocol": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "`tcp`, `udp` or `icmp`.",
						},
						"source": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "CIDR the rule applies to, e.g. `0.0.0.0/0` or " +
								"`203.0.113.4/32`.",
						},
						"port_from": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "First port. Omit for `icmp`.",
						},
						"port_to": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "Last port; omit for a single port.",
						},
					},
				},
			},
		},
	}
}

func (r *firewallResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan firewallModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.FirewallCreateWithResponse(ctx, &fbapi.FirewallCreateParams{},
		fbapi.FirewallCreateInputBody{Name: plan.Name.ValueString()})
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the firewall", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the firewall",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	plan.ID = types.StringValue(out.JSON201.Id)
	plan.CreatedAt = types.StringValue(out.JSON201.CreatedAt.Format(timeFormat))
	// State before the rules: the firewall exists now, and a rule set that
	// fails to apply must not leave an unmanaged empty firewall behind --
	// which, being deny-all, is the one that locks a machine out.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.putRules(ctx, plan.ID.ValueString(), plan.Rules, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *firewallResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state firewallModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.FirewallGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the firewall", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the firewall",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	state.Name = types.StringValue(out.JSON200.Firewall.Name)
	state.CreatedAt = types.StringValue(out.JSON200.Firewall.CreatedAt.Format(timeFormat))
	state.Rules = nil
	rules := []fbapi.FirewallRuleBody{}
	if out.JSON200.Rules != nil {
		rules = *out.JSON200.Rules
	}
	for _, rule := range rules {
		state.Rules = append(state.Rules, firewallRuleModel{
			Direction: types.StringValue(string(rule.Direction)),
			Protocol:  types.StringValue(string(rule.Protocol)),
			Source:    types.StringValue(rule.Source),
			PortFrom:  optInt64(rule.PortFrom),
			PortTo:    optInt64(rule.PortTo),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *firewallResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state firewallModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if !plan.Name.Equal(state.Name) {
		out, err := r.client.API.FirewallUpdateWithResponse(ctx, id,
			fbapi.FirewallUpdateInputBody{Name: plan.Name.ValueString()})
		if err != nil {
			apiError(&resp.Diagnostics, "Renaming the firewall", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Renaming the firewall",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
	}
	if !r.putRules(ctx, id, plan.Rules, &resp.Diagnostics) {
		return
	}
	plan.ID, plan.CreatedAt = state.ID, state.CreatedAt
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// putRules replaces the whole set. It is called on every update even when the
// rules did not change: the call is idempotent, and comparing two nested-block
// lists by hand to skip it is more code and one more place to be wrong.
func (r *firewallResource) putRules(ctx context.Context, id string, rules []firewallRuleModel, diags *diagSink) bool {
	converted := make([]fbapi.FirewallRuleBody, 0, len(rules))
	for i, rule := range rules {
		fr := fbapi.FirewallRuleBody{
			Direction: fbapi.FirewallRuleBodyDirection(rule.Direction.ValueString()),
			Protocol:  fbapi.FirewallRuleBodyProtocol(rule.Protocol.ValueString()),
			Source:    rule.Source.ValueString(),
		}
		if !rule.PortFrom.IsNull() {
			fr.PortFrom = ptr(rule.PortFrom.ValueInt64())
		}
		if !rule.PortTo.IsNull() {
			fr.PortTo = ptr(rule.PortTo.ValueInt64())
		}
		// A port range with only one end is the kind of thing the API refuses
		// with a message about a field index; saying which rule is clearer.
		if fr.PortFrom == nil && fr.PortTo != nil {
			diags.AddError("Incomplete port range",
				fmt.Sprintf("rule %d sets port_to without port_from.", i))
			return false
		}
		converted = append(converted, fr)
	}
	body := fbapi.FirewallRulesReplaceJSONRequestBody{Rules: &converted}
	out, err := r.client.API.FirewallRulesReplaceWithResponse(ctx, id, body)
	if err != nil {
		apiError(diags, "Replacing the firewall rules", err)
		return false
	}
	if out.StatusCode() >= 400 {
		apiError(diags, "Replacing the firewall rules",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	return true
}

func (r *firewallResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state firewallModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.FirewallDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the firewall", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the firewall",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *firewallResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// ============================== floating ip ==============================

var (
	_ resource.Resource                = (*floatingIPResource)(nil)
	_ resource.ResourceWithConfigure   = (*floatingIPResource)(nil)
	_ resource.ResourceWithImportState = (*floatingIPResource)(nil)
)

func NewFloatingIPResource() resource.Resource { return &floatingIPResource{} }

type floatingIPResource struct{ resourceConfigure }

type floatingIPModel struct {
	ID        types.String `tfsdk:"id"`
	Region    types.String `tfsdk:"region"`
	ServerID  types.String `tfsdk:"server_id"`
	Address   types.String `tfsdk:"address"`
	State     types.String `tfsdk:"state"`
	CreatedAt types.String `tfsdk:"created_at"`
}

func (r *floatingIPResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_floating_ip"
}

func (r *floatingIPResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A public address that can be moved between servers without changing " +
			"DNS.\n\n" +
			"It is billed while it EXISTS, attached or not, and its whole point is to outlive " +
			"the server behind it: destroying this resource releases the address, and the " +
			"address is not reserved for you afterwards.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Region slug. An address belongs to its region and can only be " +
					"attached to a server in the same one.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"server_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The server the address points at. Changing it moves the address " +
					"in place, which is the whole feature; leaving it out detaches.",
			},
			"address": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The IPv4 address, assigned at creation and stable for the resource's life.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{Computed: true},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *floatingIPResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan floatingIPModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var body fbapi.FloatingIpCreateInputBody
	if v := plan.Region.ValueString(); v != "" && !plan.Region.IsUnknown() {
		body.Region = &v
	}
	out, err := r.client.API.FloatingIpCreateWithResponse(ctx, &fbapi.FloatingIpCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the floating IP", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the floating IP",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyFloatingIP(&plan, out.JSON201)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if v := plan.ServerID.ValueString(); v != "" {
		if !r.attach(ctx, plan.ID.ValueString(), v, &resp.Diagnostics) {
			return
		}
		plan.ServerID = types.StringValue(v)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *floatingIPResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state floatingIPModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.FloatingIpGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the floating IP", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the floating IP",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyFloatingIP(&state, out.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *floatingIPResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state floatingIPModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if !plan.ServerID.Equal(state.ServerID) {
		if v := plan.ServerID.ValueString(); v != "" {
			// Attach directly, without detaching first: the API moves an
			// attached address in one call, and a detach in between would put
			// the address on nothing for as long as the second call takes,
			// which is a real outage for whatever answers on it.
			if !r.attach(ctx, id, v, &resp.Diagnostics) {
				return
			}
		} else {
			out, err := r.client.API.FloatingIpDetachWithResponse(ctx, id)
			if err != nil {
				apiError(&resp.Diagnostics, "Detaching the floating IP", err)
				return
			}
			if out.StatusCode() >= 400 {
				apiError(&resp.Diagnostics, "Detaching the floating IP",
					problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
				return
			}
		}
	}
	plan.ID, plan.Address, plan.CreatedAt = state.ID, state.Address, state.CreatedAt
	plan.Region, plan.State = state.Region, state.State
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPResource) attach(ctx context.Context, id, serverID string, diags *diagSink) bool {
	out, err := r.client.API.FloatingIpAttachWithResponse(ctx, id,
		fbapi.FloatingIpAttachInputBody{ServerId: serverID})
	if err != nil {
		apiError(diags, "Attaching the floating IP", err)
		return false
	}
	if out.StatusCode() >= 400 {
		apiError(diags, "Attaching the floating IP",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	return true
}

func (r *floatingIPResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state floatingIPModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.FloatingIpDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the floating IP", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the floating IP",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *floatingIPResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func applyFloatingIP(m *floatingIPModel, b *fbapi.FloatingIPBody) {
	m.ID = types.StringValue(b.Id)
	m.Address = types.StringValue(b.Address)
	m.State = types.StringValue(string(b.State))
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.Region = optString(b.RegionSlug)
	if b.Server != nil {
		m.ServerID = types.StringValue(b.Server.Id)
	} else {
		m.ServerID = types.StringNull()
	}
}

// ============================== dns zone ==============================

var (
	_ resource.Resource                = (*dnsZoneResource)(nil)
	_ resource.ResourceWithConfigure   = (*dnsZoneResource)(nil)
	_ resource.ResourceWithImportState = (*dnsZoneResource)(nil)
)

func NewDNSZoneResource() resource.Resource { return &dnsZoneResource{} }

type dnsZoneResource struct{ resourceConfigure }

type dnsZoneModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	ProjectID   types.String `tfsdk:"project_id"`
	Tags        types.Set    `tfsdk:"tags"`
	Nameservers types.List   `tfsdk:"nameservers"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

func (r *dnsZoneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

func (r *dnsZoneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A DNS zone hosted on the platform's nameservers.\n\n" +
			"Creating a zone does NOT prove you own the domain, and it does not make the world " +
			"ask us about it: point the domain's nameservers at the values in `nameservers` " +
			"for anything here to be answered.\n\n" +
			"**Destroying a zone destroys its records with it.** The zone name is also global: " +
			"a name in use elsewhere on the platform is refused.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The zone's apex, e.g. `example.com`. No update exists, so a change replaces it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"project_id": projectAttribute("zone"),
			"tags":       tagsAttribute("zone"),
			"nameservers": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "The nameservers to delegate the domain to at its registrar.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *dnsZoneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dnsZoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := fbapi.ZoneCreateInputBody{Name: plan.Name.ValueString()}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	body.Tags = tagsFromPlan(ctx, plan.Tags, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DnsZoneCreateWithResponse(ctx, &fbapi.DnsZoneCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the DNS zone", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the DNS zone",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	resp.Diagnostics.Append(applyDNSZone(ctx, &plan, &out.JSON201.Zone)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsZoneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dnsZoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DnsZoneGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the DNS zone", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the DNS zone",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	resp.Diagnostics.Append(applyDNSZone(ctx, &state, &out.JSON200.Zone)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update handles the two attributes a zone CAN change: its project and its
// tags. The zone's NAME is still fixed at birth and requires replacement, which
// is why this used to be an error outright — the grouping endpoints
// (2026-08-27) are what gave it something to do. That matters here more than
// elsewhere: replacing a zone deletes its records.
func (r *dnsZoneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state dnsZoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if !applyGrouping(ctx, &resp.Diagnostics, groupingUpdate{
		Noun: "zone", PlanTags: plan.Tags, StateTags: state.Tags,
		PlanProject: plan.ProjectID, StateProject: state.ProjectID,
		SetTags: func(ctx context.Context, b fbapi.TagsBody) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.DnsZoneTagsSetWithResponse(ctx, id, b)
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
		SetProject: func(ctx context.Context, pid *string) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.DnsZoneProjectSetWithResponse(ctx, id,
				fbapi.DnsZoneProjectSetJSONRequestBody{ProjectId: pid})
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
	}) {
		return
	}
	out, err := r.client.API.DnsZoneGetWithResponse(ctx, id)
	if err != nil {
		apiError(&resp.Diagnostics, "Re-reading the DNS zone", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Re-reading the DNS zone",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	resp.Diagnostics.Append(applyDNSZone(ctx, &plan, &out.JSON200.Zone)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsZoneResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dnsZoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DnsZoneDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the DNS zone", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the DNS zone",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *dnsZoneResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// ============================== dns record ==============================

var (
	_ resource.Resource                = (*dnsRecordResource)(nil)
	_ resource.ResourceWithConfigure   = (*dnsRecordResource)(nil)
	_ resource.ResourceWithImportState = (*dnsRecordResource)(nil)
)

func NewDNSRecordResource() resource.Resource { return &dnsRecordResource{} }

type dnsRecordResource struct{ resourceConfigure }

type dnsRecordModel struct {
	ID       types.String `tfsdk:"id"`
	ZoneID   types.String `tfsdk:"zone_id"`
	Name     types.String `tfsdk:"name"`
	Type     types.String `tfsdk:"type"`
	Content  types.String `tfsdk:"content"`
	TTL      types.Int64  `tfsdk:"ttl"`
	Priority types.Int64  `tfsdk:"priority"`
}

func (r *dnsRecordResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_record"
}

func (r *dnsRecordResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One record inside a `firstboot_dns_zone`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"zone_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The zone this record belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "A relative label, `@` for the apex, or a fully qualified name " +
					"inside the zone. The API's update cannot change it, so a change replaces " +
					"the record.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`A`, `AAAA`, `CNAME`, `MX`, `TXT`, `NS`, `SRV` or `CAA`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"content": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The record's value. Changed in place.",
			},
			"ttl": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(3600),
				MarkdownDescription: "Seconds, 60 to 604800.",
			},
			"priority": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Required for `MX` and `SRV`, 0 to 65535. Ignored otherwise.",
			},
		},
	}
}

func (r *dnsRecordResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dnsRecordModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := fbapi.RecordCreateInputBody{
		Name:    plan.Name.ValueString(),
		Type:    plan.Type.ValueString(),
		Content: plan.Content.ValueString(),
		Ttl:     ptr(plan.TTL.ValueInt64()),
	}
	if !plan.Priority.IsNull() {
		body.Priority = ptr(int32(plan.Priority.ValueInt64()))
	}
	out, err := r.client.API.DnsRecordCreateWithResponse(ctx, plan.ZoneID.ValueString(),
		&fbapi.DnsRecordCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the DNS record", err)
		return
	}
	if out.JSON201 == nil {
		apiError(&resp.Diagnostics, "Creating the DNS record",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyDNSRecord(&plan, out.JSON201)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read walks the zone. There is no per-record GET, so the zone's own detail
// endpoint is the only way to refresh one -- which is also why a record that no
// longer appears there is treated as deleted rather than as an error.
func (r *dnsRecordResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dnsRecordModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DnsZoneGetWithResponse(ctx, state.ZoneID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the DNS record", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			// The zone is gone, so the record is too.
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the DNS record",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	records := []fbapi.DnsRecordBody{}
	if out.JSON200.Records != nil {
		records = *out.JSON200.Records
	}
	for _, rec := range records {
		if rec.Id == state.ID.ValueString() {
			applyDNSRecord(&state, &rec)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *dnsRecordResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state dnsRecordModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := fbapi.RecordUpdateInputBody{
		Content: plan.Content.ValueString(),
		Ttl:     ptr(plan.TTL.ValueInt64()),
	}
	if !plan.Priority.IsNull() {
		body.Priority = ptr(int32(plan.Priority.ValueInt64()))
	}
	out, err := r.client.API.DnsRecordUpdateWithResponse(ctx,
		state.ZoneID.ValueString(), state.ID.ValueString(), body)
	if err != nil {
		apiError(&resp.Diagnostics, "Updating the DNS record", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Updating the DNS record",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyDNSRecord(&plan, out.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsRecordResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dnsRecordModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DnsRecordDeleteWithResponse(ctx,
		state.ZoneID.ValueString(), state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the DNS record", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the DNS record",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

// ImportState takes `zone_id/record_id`, because a record id alone cannot be
// read: the only way to fetch one is through its zone.
func (r *dnsRecordResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Unexpected import id",
			fmt.Sprintf("Expected `zone_id/record_id`, got %q.\n\n"+
				"A record has no endpoint of its own -- it is read through its zone -- so the "+
				"zone's id has to come with it.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("zone_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
}

func applyDNSRecord(m *dnsRecordModel, b *fbapi.DnsRecordBody) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.Type = types.StringValue(b.Type)
	m.Content = types.StringValue(b.Content)
	m.TTL = types.Int64Value(int64(b.Ttl))
	if b.Priority != nil {
		m.Priority = types.Int64Value(int64(*b.Priority))
	} else {
		m.Priority = types.Int64Null()
	}
}
