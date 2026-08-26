package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	firstboot "github.com/firstboot-io/firstboot-go"
	"github.com/firstboot-io/firstboot-go/fbapi"
)

// Shared plumbing. Every resource needs the same three things and getting any
// of them subtly wrong is how a provider becomes untrustworthy: the client out
// of provider data, an API refusal turned into a diagnostic a human can act on,
// and a 404 turned into "remove it from state" rather than into an error.

// configureClient pulls the client out of whatever the framework handed the
// resource. The nil check is not defensive noise: the framework calls Configure
// with nil ProviderData during its own validation walk, and a resource that
// does not tolerate it panics before the user sees a single diagnostic.
func configureClient(data any, diags *diag.Diagnostics) *firstboot.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*firstboot.Client)
	if !ok {
		diags.AddError("Unexpected provider data",
			fmt.Sprintf("Expected *firstboot.Client, got %T. This is a bug in the provider.", data))
		return nil
	}
	return c
}

// resourceConfigure is the identical Configure every resource implements.
type resourceConfigure struct {
	client *firstboot.Client
}

func (r *resourceConfigure) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = configureClient(req.ProviderData, &resp.Diagnostics)
}

type datasourceConfigure struct {
	client *firstboot.Client
}

func (d *datasourceConfigure) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = configureClient(req.ProviderData, &resp.Diagnostics)
}

// apiError turns a refusal into a diagnostic worth reading.
//
// The default -- printing err.Error() -- gives an operator a code and no idea
// what to do. These are the codes where the difference between two of them
// changes the fix, and where a generic message actively misleads: NO_CAPACITY
// tells someone to wait, PLAN_NOT_OFFERED tells them waiting will never help,
// and until 2026-08 those were the same 503.
func apiError(diags *diag.Diagnostics, action string, err error) {
	var apiErr *firstboot.APIError
	if !errors.As(err, &apiErr) {
		diags.AddError(action+" failed", err.Error())
		return
	}

	summary := action + " failed"
	detail := apiErr.Error()

	switch apiErr.Code {
	case "NO_CAPACITY_IN_REGION":
		summary = action + " failed: the region is full"
		detail = "Every host in this region that sells this plan is at capacity. " +
			"This is temporary: try again later, or choose another region.\n\n" + apiErr.Detail
	case "PLAN_NOT_OFFERED_IN_REGION":
		summary = action + " failed: that plan is not sold in that region"
		detail = "No host in this region carries this plan, so waiting will not help. " +
			"Use the `firstboot_plans` data source to see what the region actually offers.\n\n" + apiErr.Detail
	case "INSUFFICIENT_BALANCE":
		summary = action + " failed: not enough balance"
		detail = "The first month is charged upfront and the wallet cannot cover it. " +
			"Top up and re-run apply; nothing was created.\n\n" + apiErr.Detail
	case "CREATE_COOLDOWN":
		summary = action + " failed: too many resources created in the last hour"
		detail = "The account's create rate ceiling was reached. The provider already waited " +
			"as long as its retry budget allows.\n\n" +
			"For a configuration that legitimately creates many machines at once, ask support " +
			"to raise `servers.create_rate` for the account: it is an operator-editable " +
			"quota, not a fixed limit.\n\n" + apiErr.Detail
	case "IDEMPOTENCY_KEY_REUSED":
		summary = action + " failed: idempotency key reused"
		detail = "This is a provider bug rather than a configuration problem: the same " +
			"idempotency key reached the API with two different request bodies. " +
			"Please report it with the configuration that produced it.\n\n" + apiErr.Detail
	case "ORGANIZATION_SUSPENDED":
		summary = action + " failed: the account is suspended"
		detail = "No new resources are provisioned while an account is suspended. " +
			"Existing ones are unaffected.\n\n" + apiErr.Detail
	}

	if apiErr.RequestID != "" {
		// The join key between what the operator saw and what support can look
		// up. Quoting it turns a support ticket from a guess into a lookup.
		detail += fmt.Sprintf("\n\nRequest ID: %s", apiErr.RequestID)
	}
	diags.AddError(summary, detail)
}

// gone reports whether a response means the resource no longer exists.
//
// Terraform's contract for Read is that a missing resource is removed from
// state rather than being an error: the alternative is an apply that can never
// succeed again because the state names something deleted outside Terraform.
func gone(status int) bool { return status == http.StatusNotFound }

// stateErrorDiagnostic reports a resource that converged into a failure. It is
// separate from apiError because nothing about the API call failed: every
// request succeeded and the WORK did not, which is a different sentence and
// usually a different fix.
func stateErrorDiagnostic(diags *diag.Diagnostics, err error) bool {
	var se *firstboot.StateError
	if !errors.As(err, &se) {
		return false
	}
	detail := fmt.Sprintf("The %s reached the state %q, which is terminal.", se.Kind, se.State)
	if se.Code != "" {
		detail += fmt.Sprintf("\n\nError code: %s", se.Code)
	}
	detail += "\n\nThe resource EXISTS and is in Terraform state; it is the provisioning that " +
		"failed. Look at it in the panel before running apply again: destroying and " +
		"recreating is often right, but not always."
	diags.AddError("The resource did not provision successfully", detail)
	return true
}

// timeoutDiagnostic reports a wait that ran out of budget, and says what to do.
func timeoutDiagnostic(diags *diag.Diagnostics, err error) bool {
	var te *firstboot.TimeoutError
	if !errors.As(err, &te) {
		return false
	}
	diags.AddError("Timed out waiting for the resource",
		fmt.Sprintf("Gave up after %s; the last state seen was %q.\n\n"+
			"The resource was created and IS in Terraform state, so a re-run will pick it up "+
			"rather than create a second one. If it is still converging, run apply again.",
			te.Waited, te.LastState))
	return true
}

// waitError routes whichever of the three a waiter returned.
func waitError(diags *diag.Diagnostics, action string, err error) {
	if stateErrorDiagnostic(diags, err) {
		return
	}
	if timeoutDiagnostic(diags, err) {
		return
	}
	apiError(diags, action, err)
}

// ptr is the pointer-taking the generated optional fields need everywhere.
func ptr[T any](v T) *T { return &v }

// derefString reads an optional string the API may omit.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// problem pulls the parsed problem document off a generated response so
// apiError has a code to branch on.
func problem(status int, model *fbapi.ErrorModel, hdr http.Header) error {
	e := &firstboot.APIError{Status: status}
	if hdr != nil {
		e.RequestID = hdr.Get("X-Request-Id")
	}
	if model != nil {
		if model.Title != nil {
			e.Title = *model.Title
		}
		if model.Detail != nil {
			e.Detail = *model.Detail
			e.Code = firstboot.CodeFromDetail(*model.Detail)
		}
	}
	return e
}

// diagSink is what the helpers above take. Named rather than spelled
// *diag.Diagnostics at every call site because a resource, a data source and a
// plan modifier each carry their own response struct and all three hold the
// same type.
type diagSink = diag.Diagnostics

// optString renders an optional API string as a Terraform value: absent becomes
// null rather than "", so a configuration that omits an attribute matches what
// the API says about it and the plan stays empty.
func optString(s *string) types.String {
	if s == nil || *s == "" {
		return types.StringNull()
	}
	return types.StringValue(*s)
}

// optInt64 is the same for an optional number.
func optInt64(v *int64) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*v)
}

// uuidOf parses an id the API already validated. A malformed one cannot reach
// here -- it came out of a response -- so a parse failure yields the zero UUID
// and the call it feeds fails with the API's own 404 rather than a panic.
func uuidOf(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// applyDNSZone fills a zone model, including the computed nameserver list.
func applyDNSZone(ctx context.Context, m *dnsZoneModel, b *fbapi.DnsZoneBody) diag.Diagnostics {
	m.ID = types.StringValue(b.Id)
	m.Name = types.StringValue(b.Name)
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(timeFormat))
	m.ProjectID = optString(b.ProjectId)
	var ns []string
	if b.Nameservers != nil {
		ns = *b.Nameservers
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, ns)
	m.Nameservers = list
	return diags
}

// stringList reads a list attribute into a plain slice. A null or unknown list
// yields an empty slice rather than an error: "no backends" and "not configured"
// are the same request to an endpoint that replaces the whole set.
func stringList(ctx context.Context, v types.List, diags *diag.Diagnostics) ([]string, bool) {
	if v.IsNull() || v.IsUnknown() {
		return []string{}, true
	}
	var out []string
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	if diags.HasError() {
		return nil, false
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}

// Immutability where REPLACEMENT is not the answer.
//
// The framework's RequiresReplace covers the usual case: the API cannot change
// the attribute, so Terraform destroys the resource and builds a new one. A
// domain registration breaks that, because it has no destroy -- the registry
// does not take a name back, and forgetting one is not the same as releasing it.
// Replacing a domain on a renamed attribute would therefore quietly abandon a
// paid registration that keeps renewing, and buy a second one beside it.
//
// So these refuse instead. The plan fails with a sentence saying what happened
// and what to do, which costs the operator one apply and cannot cost them a
// domain.

type immutableString struct{ why string }

func (m immutableString) Description(context.Context) string { return m.why }
func (m immutableString) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m immutableString) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	// Nothing to compare on create or destroy.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if req.PlanValue.Equal(req.StateValue) {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "This attribute cannot be changed", m.why)
}

type immutableInt64 struct{ why string }

func (m immutableInt64) Description(context.Context) string { return m.why }
func (m immutableInt64) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m immutableInt64) PlanModifyInt64(_ context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if req.PlanValue.Equal(req.StateValue) {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "This attribute cannot be changed", m.why)
}

var (
	_ planmodifier.String = immutableString{}
	_ planmodifier.Int64  = immutableInt64{}
)

// optTime renders an optional timestamp the API may omit. Null rather than an
// empty string, for the same reason as optString: an absent value and a value
// that is empty are different answers.
func optTime(t *time.Time) types.String {
	if t == nil {
		return types.StringNull()
	}
	return types.StringValue(t.Format(timeFormat))
}

// settingRefused reports a non-2xx from a call that answers with nothing but a
// status.
//
// It takes the pieces rather than the generated response because those are four
// different types with no common interface. It is called only AFTER the
// transport error has been checked: a transport error leaves the generated
// response nil, and `out.StatusCode()` on a nil response panics rather than
// answering zero.
func settingRefused(diags *diag.Diagnostics, action string, status int, model *fbapi.ErrorModel, resp *http.Response) {
	var hdr http.Header
	if resp != nil {
		hdr = resp.Header
	}
	apiError(diags, action, problem(status, model, hdr))
}

// setIfConfigured copies a configured string into an optional request field and
// leaves the field nil otherwise.
//
// The unknown check is the point of it. A Computed attribute the configuration
// did not set arrives at Create as UNKNOWN, and ValueString() renders that as
// "" -- which, sent as a present-but-empty field, is a request to CLEAR the
// value rather than to leave it alone. The two are different answers on every
// endpoint here that replaces a whole body.
func setIfConfigured(v types.String, dst **string) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	s := v.ValueString()
	if s == "" {
		return
	}
	*dst = &s
}

// preferAPI takes the API's value when it sent one, and keeps what the model
// already holds otherwise.
//
// It exists for the 202 create bodies. A create answers BEFORE the resource is
// fully materialised, so a field like `region` or `plan` can be absent from that
// first body and present in every later read. Writing the absence into state
// would be wrong twice over: for a value the configuration SET, Terraform
// rejects the apply as an inconsistent result, and for a computed one it
// publishes a null that the next refresh immediately contradicts.
//
// An unknown current value becomes null rather than staying unknown, because an
// unknown cannot be written to state at all.
func preferAPI(current types.String, v *string) types.String {
	if v != nil && *v != "" {
		return types.StringValue(*v)
	}
	if !current.IsUnknown() {
		return current
	}
	return types.StringNull()
}
