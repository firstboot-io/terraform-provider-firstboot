package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/firstboot-go/fbapi"
)

var (
	_ resource.Resource                = (*loadBalancerResource)(nil)
	_ resource.ResourceWithConfigure   = (*loadBalancerResource)(nil)
	_ resource.ResourceWithImportState = (*loadBalancerResource)(nil)
)

func NewLoadBalancerResource() resource.Resource { return &loadBalancerResource{} }

type loadBalancerResource struct{ resourceConfigure }

type lbRuleModel struct {
	EntryPort  types.Int64  `tfsdk:"entry_port"`
	Protocol   types.String `tfsdk:"protocol"`
	TargetPort types.Int64  `tfsdk:"target_port"`
}

type lbHealthCheckModel struct {
	Protocol        types.String `tfsdk:"protocol"`
	Path            types.String `tfsdk:"path"`
	IntervalSeconds types.Int64  `tfsdk:"interval_seconds"`
}

type loadBalancerModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	NetworkID        types.String `tfsdk:"network_id"`
	ProjectID        types.String `tfsdk:"project_id"`
	Tags             types.Set    `tfsdk:"tags"`
	Plan             types.String `tfsdk:"plan"`
	Region           types.String `tfsdk:"region"`
	Algorithm        types.String `tfsdk:"algorithm"`
	RestrictBackends types.Bool   `tfsdk:"restrict_backends"`
	BackendIDs       types.List   `tfsdk:"backend_ids"`
	WaitFor          types.Bool   `tfsdk:"wait_for_ready"`

	Rules       []lbRuleModel       `tfsdk:"rule"`
	HealthCheck *lbHealthCheckModel `tfsdk:"health_check"`

	IP           types.String `tfsdk:"ip"`
	State        types.String `tfsdk:"state"`
	PendingApply types.Bool   `tfsdk:"pending_apply"`
	HealthyCount types.Int64  `tfsdk:"healthy_count"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func (r *loadBalancerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_load_balancer"
}

func (r *loadBalancerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A load balancer that spreads traffic across servers.\n\n" +
			"**It reaches its backends over a PRIVATE network, which is why `network_id` is " +
			"required and why every backend must already be a member of it.** Public addresses " +
			"are not reachable from here: tenant isolation flags each guest's public bridge " +
			"port. Joining a network needs a restart the customer schedules, so a server has to " +
			"be on the network before it can be a backend -- put `network_id` on the " +
			"`firstboot_server` too, and Terraform's dependency graph does the rest.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Hostname format: lowercase letters, digits and hyphens. " +
					"The API has no rename, so a change replaces the load balancer, which means " +
					"a NEW public address.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"project_id": projectAttribute("load balancer"),
			"tags":       tagsAttribute("load balancer"),
			"network_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The private network the load balancer reaches its backends over. " +
					"It cannot be moved to another network, so a change replaces the resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"plan": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`lb-1`, `lb-2` or `lb-3`; defaults to `lb-1`.\n\n" +
					"**There is no resize.** A plan change replaces the load balancer and its " +
					"address with it, which is why this forces replacement rather than pretending " +
					"to apply in place.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"region": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Region slug; defaults to the platform's first active region. " +
					"It has to be the network's region, and it cannot be changed afterwards.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"algorithm": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "`round_robin` (the default) or `least_connections`. " +
					"Applied in place, together with the rules and the health check.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"restrict_backends": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Close each backend's target port to everything except this load " +
					"balancer. Default `true`, and worth leaving on: without it the backends stay " +
					"reachable directly and the load balancer is a suggestion rather than a door.\n\n" +
					"**Write-only.** The API does not return it, so a change here cannot be " +
					"detected and cannot be applied; it forces replacement instead of promising " +
					"something the apply would not do.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"backend_ids": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				MarkdownDescription: "Servers behind the load balancer, each already a member of " +
					"`network_id`. Replaced as a whole set, in place.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the load balancer to finish provisioning.",
			},
			"health_check": schema.SingleNestedAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "How a backend is judged healthy. Omit it for a TCP check every " +
					"10 seconds, which is what the platform applies.",
				Attributes: map[string]schema.Attribute{
					"protocol": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "`tcp` (the default) or `http`.",
					},
					"path": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "The path an `http` check requests. Defaults to `/`; ignored for `tcp`.",
					},
					"interval_seconds": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "5 to 300; defaults to 10.",
					},
				},
				PlanModifiers: []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
			},

			"ip": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The public address traffic arrives on. Assigned during " +
					"provisioning, so it is only meaningful once `wait_for_ready` has returned.",
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`active`, `stopped_dunning` and `deleted` are settled; " +
					"an `error_*` value means provisioning failed.",
			},
			"pending_apply": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "True while an edit has not reached the data plane yet. " +
					"The state stays `active` throughout an edit, so this is the only field that " +
					"moves when rules or backends change.",
			},
			"healthy_count": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "How many backends passed their last probe. Zero on a load " +
					"balancer that was just created: the first probe has not run.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
		Blocks: map[string]schema.Block{
			// A block rather than one resource per rule, for the same reason as
			// the firewall's: the API replaces the whole set in one call, so two
			// rule resources applying concurrently would lose rules.
			"rule": schema.ListNestedBlock{
				MarkdownDescription: "One forwarding rule. The whole set is replaced on every change.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"entry_port": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "The port traffic arrives on.",
						},
						"protocol": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "`http` adds an `X-Forwarded-For` header; `tcp` passes the " +
								"connection through untouched, which is what HTTPS passthrough needs.",
						},
						"target_port": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "The port on each backend.",
						},
					},
				},
			},
		},
	}
}

func (r *loadBalancerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan loadBalancerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := fbapi.LoadBalancerCreateInputBody{
		Name:             plan.Name.ValueString(),
		NetworkId:        plan.NetworkID.ValueString(),
		RestrictBackends: ptr(plan.RestrictBackends.ValueBool()),
	}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	body.Tags = tagsFromPlan(ctx, plan.Tags, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if v := plan.Plan.ValueString(); v != "" && !plan.Plan.IsUnknown() {
		body.Plan = &v
	}
	if v := plan.Region.ValueString(); v != "" && !plan.Region.IsUnknown() {
		body.Region = &v
	}
	if v := plan.Algorithm.ValueString(); v != "" && !plan.Algorithm.IsUnknown() {
		a := fbapi.LoadBalancerCreateInputBodyAlgorithm(v)
		body.Algorithm = &a
	}
	ids, ok := stringList(ctx, plan.BackendIDs, &resp.Diagnostics)
	if !ok {
		return
	}
	if len(ids) > 0 {
		body.BackendIds = &ids
	}
	// `backend_ids` is Optional AND Computed, so a configuration that omits it
	// arrives here UNKNOWN -- and an unknown value cannot be written to state.
	// The create response carries the load balancer alone, without its backend
	// list, so the known answer has to come from what was just sent.
	backendList, d := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.BackendIDs = backendList
	rules := lbRulesFrom(plan.Rules)
	body.Rules = &rules
	if hc := lbHealthCheckFrom(plan.HealthCheck); hc != nil {
		body.HealthCheck = hc
	}

	out, err := r.client.API.LoadBalancerCreateWithResponse(ctx, &fbapi.LoadBalancerCreateParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Creating the load balancer", err)
		return
	}
	if out.JSON202 == nil {
		apiError(&resp.Diagnostics, "Creating the load balancer",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}

	id := out.JSON202.Id
	applyLoadBalancer(ctx, &plan, out.JSON202, nil, &resp.Diagnostics)
	// State before the wait: an interrupted apply must not leave a billing load
	// balancer that no state file names.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() || !plan.WaitFor.ValueBool() {
		return
	}
	if _, err := r.client.WaitForLoadBalancer(ctx, id); err != nil {
		waitError(&resp.Diagnostics, "Waiting for the load balancer", err)
		return
	}
	if !r.refresh(ctx, id, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state loadBalancerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.LoadBalancerGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the load balancer", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the load balancer",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	applyLoadBalancer(ctx, &state, &out.JSON200.LoadBalancer, out.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *loadBalancerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state loadBalancerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	if !applyGrouping(ctx, &resp.Diagnostics, groupingUpdate{
		Noun: "load balancer", PlanTags: plan.Tags, StateTags: state.Tags,
		PlanProject: plan.ProjectID, StateProject: state.ProjectID,
		SetTags: func(ctx context.Context, b fbapi.TagsBody) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.LoadBalancerTagsSetWithResponse(ctx, id, b)
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
		SetProject: func(ctx context.Context, pid *string) (int, *fbapi.ErrorModel, http.Header, error) {
			out, err := r.client.API.LoadBalancerProjectSetWithResponse(ctx, id,
				fbapi.LoadBalancerProjectSetJSONRequestBody{ProjectId: pid})
			if err != nil {
				return 0, nil, nil, err
			}
			return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header, nil
		},
	}) {
		return
	}

	// Backends first, rules second. A rule set that starts forwarding to a port
	// before the servers behind it are attached would answer with a connection
	// refused; the other order only ever leaves a backend receiving nothing,
	// which is what it was already doing.
	if !plan.BackendIDs.Equal(state.BackendIDs) {
		ids, ok := stringList(ctx, plan.BackendIDs, &resp.Diagnostics)
		if !ok {
			return
		}
		out, err := r.client.API.LoadBalancerBackendsSetWithResponse(ctx, id,
			fbapi.LoadBalancerBackendsPutInputBody{ServerIds: &ids})
		if err != nil {
			apiError(&resp.Diagnostics, "Setting the load balancer backends", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Setting the load balancer backends",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
	}

	// The API replaces the algorithm, the health check and the rules together in
	// one call, so a change to any of the three sends all three. Sending them
	// unconditionally would be simpler and would also rewrite the data plane on
	// every unrelated apply, which is what pending_apply then reports.
	if !lbRulesEqual(plan.Rules, state.Rules) ||
		!plan.Algorithm.Equal(state.Algorithm) ||
		!lbHealthCheckEqual(plan.HealthCheck, state.HealthCheck) {
		rules := lbRulesFrom(plan.Rules)
		put := fbapi.LoadBalancerRulesPutInputBody{Rules: &rules}
		if v := plan.Algorithm.ValueString(); v != "" && !plan.Algorithm.IsUnknown() {
			a := fbapi.LoadBalancerRulesPutInputBodyAlgorithm(v)
			put.Algorithm = &a
		}
		if hc := lbHealthCheckFrom(plan.HealthCheck); hc != nil {
			put.HealthCheck = hc
		}
		out, err := r.client.API.LoadBalancerRulesReplaceWithResponse(ctx, id, put)
		if err != nil {
			apiError(&resp.Diagnostics, "Replacing the load balancer rules", err)
			return
		}
		if out.StatusCode() >= 400 {
			apiError(&resp.Diagnostics, "Replacing the load balancer rules",
				problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
			return
		}
	}

	if !r.refresh(ctx, id, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state loadBalancerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.LoadBalancerDeleteWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Deleting the load balancer", err)
		return
	}
	if out.StatusCode() >= 400 && !gone(out.StatusCode()) {
		apiError(&resp.Diagnostics, "Deleting the load balancer",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
	}
}

func (r *loadBalancerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.AddWarning("A write-only attribute cannot be imported",
		"`restrict_backends` is not returned by the API, so an imported load balancer has no "+
			"value for it. It defaults to `true`; if the load balancer was created with `false`, "+
			"the next plan will want to REPLACE it. Check the plan before applying.")
}

// refresh re-reads the load balancer and its rules into the model. The rules and
// backends live OUTSIDE the load balancer body, in the detail endpoint's own
// fields, so a create response alone cannot fill them.
func (r *loadBalancerResource) refresh(ctx context.Context, id string, m *loadBalancerModel, diags *diagSink) bool {
	out, err := r.client.API.LoadBalancerGetWithResponse(ctx, id)
	if err != nil {
		apiError(diags, "Re-reading the load balancer", err)
		return false
	}
	if out.JSON200 == nil {
		apiError(diags, "Re-reading the load balancer",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	applyLoadBalancer(ctx, m, &out.JSON200.LoadBalancer, out.JSON200, diags)
	return true
}

// applyLoadBalancer fills the model. `detail` is the detail endpoint's body when
// there is one; a create response carries the load balancer and nothing else, so
// rules and backends are left alone rather than blanked.
func applyLoadBalancer(ctx context.Context, m *loadBalancerModel, b *fbapi.LoadBalancerBody, detail *fbapi.LoadBalancerGetOutputBody, diags *diagSink) {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.State = types.StringValue(string(b.State))
	m.Algorithm = types.StringValue(string(b.Algorithm))
	m.PendingApply = types.BoolValue(b.PendingApply)
	m.HealthyCount = types.Int64Value(b.HealthyCount)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.IP = optString(b.Ip)
	// The create's 202 body can omit these; every later read carries them.
	m.Region = preferAPI(m.Region, b.RegionSlug)
	m.Plan = preferAPI(m.Plan, b.PlanSlug)
	if b.NetworkId != nil {
		m.NetworkID = types.StringValue(*b.NetworkId)
	}
	m.ProjectID = optString(b.ProjectId)
	applyTags(ctx, &m.Tags, b.Tags, diags)
	m.HealthCheck = &lbHealthCheckModel{
		Protocol:        types.StringValue(healthCheckProtocol(b.HealthCheck.Protocol)),
		Path:            types.StringValue(healthCheckPath(b.HealthCheck.Path)),
		IntervalSeconds: types.Int64Value(healthCheckInterval(b.HealthCheck.IntervalSeconds)),
	}

	if detail == nil {
		return
	}
	var rules []lbRuleModel
	if detail.Rules != nil {
		for _, ru := range *detail.Rules {
			rules = append(rules, lbRuleModel{
				EntryPort:  types.Int64Value(ru.EntryPort),
				Protocol:   types.StringValue(string(ru.Protocol)),
				TargetPort: types.Int64Value(ru.TargetPort),
			})
		}
	}
	m.Rules = rules

	ids := []string{}
	if detail.Backends != nil {
		for _, be := range *detail.Backends {
			ids = append(ids, be.ServerId)
		}
	}
	list, d := types.ListValueFrom(ctx, types.StringType, ids)
	diags.Append(d...)
	m.BackendIDs = list
}

// The health check's three fields are optional in the API and the API answers
// with its own defaults filled in, so these turn "absent" into the value the
// platform actually applied rather than into a null the next plan would fight.
func healthCheckProtocol(p *fbapi.LoadBalancerHealthCheckBodyProtocol) string {
	if p == nil || *p == "" {
		return string(fbapi.LoadBalancerHealthCheckBodyProtocolTcp)
	}
	return string(*p)
}

func healthCheckPath(p *string) string {
	if p == nil || *p == "" {
		return "/"
	}
	return *p
}

func healthCheckInterval(v *int64) int64 {
	if v == nil || *v == 0 {
		return 10
	}
	return *v
}

func lbRulesFrom(rules []lbRuleModel) []fbapi.LoadBalancerRuleBody {
	out := make([]fbapi.LoadBalancerRuleBody, 0, len(rules))
	for _, r := range rules {
		out = append(out, fbapi.LoadBalancerRuleBody{
			EntryPort:  r.EntryPort.ValueInt64(),
			Protocol:   fbapi.LoadBalancerRuleBodyProtocol(r.Protocol.ValueString()),
			TargetPort: r.TargetPort.ValueInt64(),
		})
	}
	return out
}

func lbRulesEqual(a, b []lbRuleModel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].EntryPort.Equal(b[i].EntryPort) ||
			!a[i].Protocol.Equal(b[i].Protocol) ||
			!a[i].TargetPort.Equal(b[i].TargetPort) {
			return false
		}
	}
	return true
}

// lbHealthCheckFrom sends only what the configuration actually set. An unknown
// value is what the framework fills a Computed attribute with before apply, and
// sending it as a zero would ask for an interval of 0 seconds.
func lbHealthCheckFrom(m *lbHealthCheckModel) *fbapi.LoadBalancerHealthCheckBody {
	if m == nil {
		return nil
	}
	out := &fbapi.LoadBalancerHealthCheckBody{}
	if v := m.Protocol.ValueString(); v != "" && !m.Protocol.IsUnknown() {
		p := fbapi.LoadBalancerHealthCheckBodyProtocol(v)
		out.Protocol = &p
	}
	if v := m.Path.ValueString(); v != "" && !m.Path.IsUnknown() {
		out.Path = &v
	}
	if v := m.IntervalSeconds.ValueInt64(); v > 0 && !m.IntervalSeconds.IsUnknown() {
		out.IntervalSeconds = &v
	}
	return out
}

func lbHealthCheckEqual(a, b *lbHealthCheckModel) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Protocol.Equal(b.Protocol) &&
		a.Path.Equal(b.Path) &&
		a.IntervalSeconds.Equal(b.IntervalSeconds)
}
