package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/firstboot-go/fbapi"
)

// A domain registration is the one resource here that cannot be undone.
//
// Everything else in this provider can be destroyed and rebuilt; a registry does
// not take a name back, and the money is spent the moment the order is accepted.
// Three consequences run through this file:
//
//   - Delete makes NO API call. It removes the resource from Terraform's state
//     and says loudly that the registration continues and keeps renewing. The
//     alternative -- an error -- would make `terraform destroy` impossible for
//     any configuration that contains a domain.
//   - The identity attributes REFUSE a change rather than forcing replacement.
//     Replacement would mean forgetting a paid name and buying another one, in
//     one apply, from a typo.
//   - The create is the endpoint where an idempotency key matters most in this
//     whole API. The SDK sets one and reuses it across its retries, which is
//     what stops a lost response from buying the same name twice.
var (
	_ resource.Resource                = (*domainResource)(nil)
	_ resource.ResourceWithConfigure   = (*domainResource)(nil)
	_ resource.ResourceWithImportState = (*domainResource)(nil)
)

func NewDomainResource() resource.Resource { return &domainResource{} }

type domainResource struct{ resourceConfigure }

type domainModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Years         types.Int64  `tfsdk:"years"`
	ContactID     types.String `tfsdk:"contact_id"`
	ProjectID     types.String `tfsdk:"project_id"`
	AutoRenew     types.Bool   `tfsdk:"auto_renew"`
	Privacy       types.Bool   `tfsdk:"privacy"`
	RegistrarLock types.Bool   `tfsdk:"registrar_lock"`
	Nameservers   types.List   `tfsdk:"nameservers"`
	WaitFor       types.Bool   `tfsdk:"wait_for_active"`

	TLD                types.String `tfsdk:"tld"`
	State              types.String `tfsdk:"state"`
	NameserversApplied types.List   `tfsdk:"nameservers_applied"`
	DNSZoneID          types.String `tfsdk:"dns_zone_id"`
	RegisteredAt       types.String `tfsdk:"registered_at"`
	ExpiresAt          types.String `tfsdk:"expires_at"`
	TransferableAt     types.String `tfsdk:"transferable_at"`
	LastErrorCode      types.String `tfsdk:"last_error_code"`
	CreatedAt          types.String `tfsdk:"created_at"`
}

func (r *domainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

func (r *domainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A domain registration.\n\n" +
			"**This resource buys something, and `terraform destroy` cannot sell it back.** " +
			"A registry does not take a name back and the money is spent when the order is " +
			"accepted, so destroying this resource only makes Terraform FORGET the domain: it " +
			"stays registered, and it keeps renewing if `auto_renew` is on. Guard it:\n\n" +
			"```hcl\n" +
			"resource \"firstboot_domain\" \"example\" {\n" +
			"  name       = \"example.com\"\n" +
			"  years      = 1\n" +
			"  contact_id = firstboot_domain_contact.main.id\n\n" +
			"  lifecycle {\n" +
			"    prevent_destroy = true\n" +
			"  }\n" +
			"}\n" +
			"```\n\n" +
			"The registrant contact has to exist before the order. There is no " +
			"`firstboot_domain_contact` resource yet -- create the profile in the panel and " +
			"pass its id.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The full name including the extension, e.g. `example.com`.\n\n" +
					"**Cannot be changed.** A different name is a different registration, and " +
					"editing this in place would mean abandoning a name that is still paid for " +
					"and still renewing. Remove the resource block and write a new one.",
				PlanModifiers: []planmodifier.String{immutableString{
					why: "A registered name cannot be changed. A different name is a different " +
						"registration: the current one stays registered and keeps renewing whatever " +
						"this configuration says.\n\n" +
						"To register another name, add a second `firstboot_domain` resource. To stop " +
						"managing this one, remove it from state with `terraform state rm` -- the " +
						"registration is unaffected either way.",
				}},
			},
			"years": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "The registration term, 1 to 10. Charged upfront from the wallet.\n\n" +
					"**Cannot be changed.** Extending a registration is a RENEWAL, which is a " +
					"second purchase rather than an edit, and this provider does not spend money " +
					"from a plan that reads like a settings change. Renew from the panel.",
				PlanModifiers: []planmodifier.Int64{immutableInt64{
					why: "The registration term cannot be edited. Extending a domain is a RENEWAL: " +
						"a second purchase charged to the wallet, not a change to an existing " +
						"registration.\n\n" +
						"Renew from the panel. Changing this number here would either do nothing or " +
						"buy years silently, and neither belongs in a plan.",
				}},
			},
			"contact_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The registrant profile the name is registered to. Create it in " +
					"the panel first.\n\n" +
					"**Cannot be changed here.** Moving a registration to another registrant is a " +
					"registry operation with its own rules and its own transfer lock; the API " +
					"offers no endpoint for it.",
				PlanModifiers: []planmodifier.String{immutableString{
					why: "The registrant of an existing registration cannot be changed through this " +
						"API. It is a registry operation with its own rules, not an attribute edit.",
				}},
			},
			"project_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional project to group the domain under, set at registration.\n\n" +
					"**Cannot be changed here.** The API has no endpoint that moves a domain " +
					"between projects; move it in the panel and the next refresh picks it up.",
				PlanModifiers: []planmodifier.String{immutableString{
					why: "The API has no endpoint that moves a domain between projects, so a change " +
						"here would be a plan promising something the apply cannot do. Move it in " +
						"the panel instead.",
				}},
			},
			"auto_renew": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the registration renews itself before expiry. Changed in place.\n\n" +
					"Turning it OFF is how a domain is allowed to lapse; there is no other way, " +
					"because nothing here can withdraw a registration.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"privacy": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "WHOIS privacy, where the registry offers it. Changed in place.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"registrar_lock": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The registrar's transfer lock. Changed in place.\n\n" +
					"Leaving it on is the cheapest anti-hijack control a domain has; it has to " +
					"come off only to transfer the name away.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"nameservers": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				MarkdownDescription: "Two to thirteen hostnames to delegate the zone to. Changed in place.\n\n" +
					"This is what you ASKED for; `nameservers_applied` is what the registrar has " +
					"confirmed. They differ while a change is in flight, and a change that never " +
					"lands stays visible as the difference between the two rather than silently " +
					"reading back as applied.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"wait_for_active": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the registry to confirm the order. With it " +
					"off, the resource exists in state while the name may still be refused -- and " +
					"a refusal is refunded, so the difference shows up as a domain that never " +
					"became `active`.",
			},

			"tld": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`active`, `expired`, `redemption` and `failed` are settled; " +
					"`registering` and `transfer_pending` mean the registry has not answered yet. " +
					"A `failed` order is refunded in full.",
			},
			"nameservers_applied": schema.ListAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "What the registrar last CONFIRMED, as opposed to what was asked for.",
			},
			"dns_zone_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The DNS zone created for this name, when the platform's own " +
					"nameservers are the ones serving it.",
			},
			"registered_at": schema.StringAttribute{Computed: true},
			"expires_at":    schema.StringAttribute{Computed: true},
			"transferable_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "When the registry's 60-day post-registration transfer lock lifts. " +
					"It is enforced by the registry, not by this platform.",
			},
			"last_error_code": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The registry's own refusal code, when there was one.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *domainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := fbapi.DomainRegisterInputBody{
		Name:      plan.Name.ValueString(),
		Years:     plan.Years.ValueInt64(),
		ContactId: plan.ContactID.ValueString(),
	}
	if v := plan.ProjectID.ValueString(); v != "" {
		body.ProjectId = &v
	}
	if !plan.AutoRenew.IsUnknown() && !plan.AutoRenew.IsNull() {
		body.AutoRenew = ptr(plan.AutoRenew.ValueBool())
	}
	if !plan.Privacy.IsUnknown() && !plan.Privacy.IsNull() {
		body.Privacy = ptr(plan.Privacy.ValueBool())
	}

	out, err := r.client.API.DomainRegisterWithResponse(ctx, &fbapi.DomainRegisterParams{}, body)
	if err != nil {
		apiError(&resp.Diagnostics, "Registering the domain", err)
		return
	}
	if out.JSON200 == nil {
		apiError(&resp.Diagnostics, "Registering the domain",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}

	id := out.JSON200.Domain.Id
	resp.Diagnostics.Append(applyDomain(ctx, &plan, &out.JSON200.Domain)...)
	// State FIRST, before anything else can fail. This one matters more than
	// anywhere else in the provider: the order is placed and the wallet is
	// already debited, and a state file that does not name the domain would
	// leave the next apply trying to buy it a second time.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.WaitFor.ValueBool() {
		if _, err := r.client.WaitForDomain(ctx, id); err != nil {
			waitError(&resp.Diagnostics, "Waiting for the registry", err)
			return
		}
	}
	// Nameservers and the lock are their own endpoints and only make sense once
	// the name is really registered, so they run after the wait.
	if !r.applySettings(ctx, id, &plan, nil, &resp.Diagnostics) {
		return
	}
	if !r.refresh(ctx, id, &plan, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *domainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.API.DomainGetWithResponse(ctx, state.ID.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "Reading the domain", err)
		return
	}
	if out.JSON200 == nil {
		if gone(out.StatusCode()) {
			resp.State.RemoveResource(ctx)
			return
		}
		apiError(&resp.Diagnostics, "Reading the domain",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return
	}
	resp.Diagnostics.Append(applyDomain(ctx, &state, &out.JSON200.Domain)...)
	// The registrant is not on the domain body; it comes back beside it.
	state.ContactID = types.StringValue(out.JSON200.Contact.Id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *domainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state domainModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	if !r.applySettings(ctx, id, &plan, &state, &resp.Diagnostics) {
		return
	}
	if !r.refresh(ctx, id, &plan, &resp.Diagnostics) {
		return
	}
	plan.ContactID = state.ContactID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete makes NO API call, because there is nothing to call: a registration
// cannot be withdrawn and a registry does not refund a name because a plan
// stopped mentioning it.
//
// So this forgets the domain and says so. The alternative -- returning an error
// -- would be more literal and would also make `terraform destroy` impossible
// for any configuration containing a domain, which pushes people towards
// `terraform state rm` anyway, just angrier.
func (r *domainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state domainModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	name := state.Name.ValueString()
	if name == "" {
		name = "the domain"
	}
	renewing := "It will NOT renew, because auto-renew is off, and it will lapse at the expiry date."
	if state.AutoRenew.ValueBool() {
		renewing = "It WILL renew automatically and charge the wallet, because auto-renew is on. " +
			"Turn auto-renew off before removing this resource if the intention was to let it lapse."
	}
	resp.Diagnostics.AddWarning("The registration was not cancelled",
		"Terraform has stopped managing "+name+", but the domain is still registered to this "+
			"account. Nothing here can withdraw a registration: the registry does not take a "+
			"name back and the term is already paid for.\n\n"+renewing+"\n\n"+
			"Add `lifecycle { prevent_destroy = true }` to a domain that must not be dropped from "+
			"a configuration by accident.")
}

func (r *domainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applySettings pushes the editable settings that are not part of the register
// call. `state` is nil on create.
//
// Auto-renew and privacy are deliberately NOT pushed on create: the register
// body already carries both, and sending them again would be a second call that
// can fail on a registry that does not sell privacy -- turning a successful
// registration into a failed apply over a setting that was already applied.
func (r *domainResource) applySettings(ctx context.Context, id string, plan, state *domainModel, diags *diagSink) bool {
	// changed answers "is this a value the API has to be told about". On update
	// that is a difference from state; on create it is anything the
	// configuration actually set, since an unknown is the framework's
	// placeholder for a Computed attribute nobody configured.
	changed := func(now, before types.Bool) bool {
		if now.IsUnknown() || now.IsNull() {
			return false
		}
		if state == nil {
			return true
		}
		return !now.Equal(before)
	}

	if state != nil {
		if changed(plan.AutoRenew, state.AutoRenew) {
			out, err := r.client.API.DomainAutoRenewSetWithResponse(ctx, id,
				fbapi.DomainAutoRenewInputBody{AutoRenew: plan.AutoRenew.ValueBool()})
			if err != nil {
				apiError(diags, "Setting auto-renew", err)
				return false
			}
			if out.StatusCode() >= 400 {
				settingRefused(diags, "Setting auto-renew", out.StatusCode(),
					out.ApplicationproblemJSONDefault, out.HTTPResponse)
				return false
			}
		}
		if changed(plan.Privacy, state.Privacy) {
			out, err := r.client.API.DomainPrivacySetWithResponse(ctx, id,
				fbapi.DomainFlagInputBody{Enabled: plan.Privacy.ValueBool()})
			if err != nil {
				apiError(diags, "Setting WHOIS privacy", err)
				return false
			}
			if out.StatusCode() >= 400 {
				settingRefused(diags, "Setting WHOIS privacy", out.StatusCode(),
					out.ApplicationproblemJSONDefault, out.HTTPResponse)
				return false
			}
		}
	}

	var lockBefore types.Bool
	if state != nil {
		lockBefore = state.RegistrarLock
	}
	if changed(plan.RegistrarLock, lockBefore) {
		out, err := r.client.API.DomainLockSetWithResponse(ctx, id,
			fbapi.DomainFlagInputBody{Enabled: plan.RegistrarLock.ValueBool()})
		if err != nil {
			apiError(diags, "Setting the registrar lock", err)
			return false
		}
		if out.StatusCode() >= 400 {
			settingRefused(diags, "Setting the registrar lock", out.StatusCode(),
				out.ApplicationproblemJSONDefault, out.HTTPResponse)
			return false
		}
	}

	setNS := !plan.Nameservers.IsUnknown() && !plan.Nameservers.IsNull()
	if state != nil {
		setNS = setNS && !plan.Nameservers.Equal(state.Nameservers)
	}
	if setNS {
		ns, ok := stringList(ctx, plan.Nameservers, diags)
		if !ok {
			return false
		}
		out, err := r.client.API.DomainNameserversSetWithResponse(ctx, id,
			fbapi.DomainNameserversInputBody{Nameservers: &ns})
		if err != nil {
			apiError(diags, "Setting the nameservers", err)
			return false
		}
		if out.StatusCode() >= 400 {
			settingRefused(diags, "Setting the nameservers", out.StatusCode(),
				out.ApplicationproblemJSONDefault, out.HTTPResponse)
			return false
		}
	}
	return true
}

func (r *domainResource) refresh(ctx context.Context, id string, m *domainModel, diags *diagSink) bool {
	out, err := r.client.API.DomainGetWithResponse(ctx, id)
	if err != nil {
		apiError(diags, "Re-reading the domain", err)
		return false
	}
	if out.JSON200 == nil {
		apiError(diags, "Re-reading the domain",
			problem(out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse.Header))
		return false
	}
	diags.Append(applyDomain(ctx, m, &out.JSON200.Domain)...)
	return true
}

func applyDomain(ctx context.Context, m *domainModel, b *fbapi.DomainBody) diagSink {
	var diags diagSink
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.TLD = types.StringValue(b.Tld)
	m.State = types.StringValue(string(b.State))
	m.Years = types.Int64Value(b.Years)
	m.AutoRenew = types.BoolValue(b.AutoRenew)
	m.Privacy = types.BoolValue(b.Privacy)
	m.RegistrarLock = types.BoolValue(b.RegistrarLock)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.DNSZoneID = optString(b.DnsZoneId)
	m.LastErrorCode = optString(b.LastErrorCode)
	m.ProjectID = optString(b.ProjectId)
	m.RegisteredAt = optTime(b.RegisteredAt)
	m.ExpiresAt = optTime(b.ExpiresAt)
	m.TransferableAt = optTime(b.TransferableAt)

	var asked, applied []string
	if b.Nameservers != nil {
		asked = *b.Nameservers
	}
	if b.NameserversApplied != nil {
		applied = *b.NameserversApplied
	}
	askedList, d := types.ListValueFrom(ctx, types.StringType, asked)
	diags.Append(d...)
	m.Nameservers = askedList
	appliedList, d := types.ListValueFrom(ctx, types.StringType, applied)
	diags.Append(d...)
	m.NameserversApplied = appliedList
	return diags
}
