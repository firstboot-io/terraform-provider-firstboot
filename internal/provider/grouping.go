package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/firstboot-io/go-sdk/fbapi"
)

// Tags: the second grouping axis, on all eight resources that have one.
//
// A resource is in at most one project (`project_id`) and wears any number of
// tags. The project axis is how a customer divides their own work; the tag axis
// is how a configuration SELECTS — which is why the plural data sources at the
// bottom of this file exist and why tags were added to the API at all.
//
// The attribute is a SET rather than a list, deliberately. The API stores tags
// sorted and deduplicated, so a list would show a diff whenever somebody wrote
// them in another order, and a set is what the value actually is.

// tagPattern mirrors `tag_array_valid` in the platform's migration 00057 and
// `grouping.TagPattern` in its Go. The provider checks it at PLAN time so the
// error names the offending tag before anything is created.
var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,31}$`)

const maxTagsPerResource = 10

// tagsAttribute is the `tags` attribute, identical on every groupable resource.
//
// Optional and NOT Computed: the configuration is the authority. A tag added in
// the panel therefore shows up as drift the next plan removes, which is the
// same contract every other managed attribute has and the only one that can be
// stated in a sentence.
func tagsAttribute(noun string) rschema.SetAttribute {
	return rschema.SetAttribute{
		Optional:    true,
		ElementType: types.StringType,
		MarkdownDescription: "Tags on this " + noun + ", used to select it from a " +
			"`firstboot_*` data source and to filter it in the panel.\n\n" +
			"At most 10, each up to 32 characters of `a-z`, `0-9`, `.`, `_`, `:` and `-`, " +
			"starting with a letter or a digit. `env:prod` is a convention, not a schema: " +
			"a tag is one flat string.\n\n" +
			"**Written exactly as given.** The API stores tags lowercased, so a tag that is " +
			"not already lowercase is refused at plan time rather than silently rewritten — " +
			"a rewritten value would differ from the configuration on every subsequent plan.",
		Validators: []validator.Set{tagSetValidator{}},
	}
}

// tagSetValidator refuses at PLAN time what the API would refuse at apply time,
// plus one thing the API accepts and Terraform cannot live with: a tag that is
// not already in stored form. The API lowercases, so `Env:Prod` would come back
// as `env:prod` and the configuration would disagree with state forever.
type tagSetValidator struct{}

func (tagSetValidator) Description(context.Context) string {
	return "at most 10 lowercase tags of [a-z0-9._:-], each starting with a letter or a digit"
}

func (v tagSetValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (tagSetValidator) ValidateSet(ctx context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	var tags []string
	resp.Diagnostics.Append(req.ConfigValue.ElementsAs(ctx, &tags, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(tags) > maxTagsPerResource {
		resp.Diagnostics.AddAttributeError(req.Path, "Too many tags",
			fmt.Sprintf("%d tags given; a resource may carry at most %d.", len(tags), maxTagsPerResource))
	}
	for _, tag := range tags {
		switch {
		case tag != strings.ToLower(tag):
			resp.Diagnostics.AddAttributeError(req.Path, "Tag is not lowercase",
				fmt.Sprintf("%q must be written as %q. The API stores tags lowercased, so writing "+
					"it this way would leave the configuration and the state permanently different.",
					tag, strings.ToLower(tag)))
		case !tagPattern.MatchString(tag):
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid tag",
				fmt.Sprintf("%q must start with a letter or a digit and use only a-z, 0-9, "+
					"dot, underscore, colon and dash, up to 32 characters.", tag))
		}
	}
}

// tagsFromPlan reads the configured tags for a create body. A null set means
// the attribute was not configured, which is not the same as an empty one and
// is sent as nothing at all.
func tagsFromPlan(ctx context.Context, v types.Set, diags *diag.Diagnostics) *[]string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	var tags []string
	diags.Append(v.ElementsAs(ctx, &tags, false)...)
	if diags.HasError() {
		return nil
	}
	// Sorted on the way out so the request body is stable between runs, which
	// matters because the SDK derives nothing from it but a human reading an
	// audit entry does.
	sort.Strings(tags)
	return &tags
}

// applyTags refreshes the attribute from what the API returned.
//
// An empty answer becomes NULL when the attribute was not configured and stays
// an empty set when it was. Collapsing both into null would make `tags = []` a
// permanent diff; collapsing both into an empty set would make every untagged
// resource in every configuration show one.
func applyTags(ctx context.Context, dst *types.Set, api *[]string, diags *diag.Diagnostics) {
	var tags []string
	if api != nil {
		tags = *api
	}
	if len(tags) == 0 && (dst.IsNull() || dst.IsUnknown()) {
		*dst = types.SetNull(types.StringType)
		return
	}
	v, d := types.SetValueFrom(ctx, types.StringType, tags)
	diags.Append(d...)
	if d.HasError() {
		return
	}
	*dst = v
}

// tagsChanged reports whether an update has to call the tags endpoint at all.
func tagsChanged(plan, state types.Set) bool { return !plan.Equal(state) }

// tagsBody builds the request body for a `PUT .../tags`. A null set clears the
// tags: the endpoint REPLACES, so sending an empty list is how a customer
// removes the last one.
func tagsBody(ctx context.Context, v types.Set, diags *diag.Diagnostics) fbapi.TagsBody {
	tags := []string{}
	if !v.IsNull() && !v.IsUnknown() {
		diags.Append(v.ElementsAs(ctx, &tags, false)...)
		sort.Strings(tags)
	}
	return fbapi.TagsBody{Tags: &tags}
}

// groupingUpdate is the two grouping changes an Update may carry, in one place.
//
// It exists because eight resources otherwise repeat the same thirty lines: is
// this changed, call the endpoint, translate the status, name the resource in
// the error. Repeating that is how one of the eight ends up not checking a
// status code, and it is a silent failure — the apply succeeds and the tag is
// simply not there.
//
// The setters are closures rather than an interface because the generated
// client gives every endpoint its own response type. Each returns the three
// things an error needs.
type groupingUpdate struct {
	// Noun names the resource in an error message ("the volume's tags").
	Noun         string
	PlanTags     types.Set
	StateTags    types.Set
	PlanProject  types.String
	StateProject types.String
	SetTags      func(context.Context, fbapi.TagsBody) (int, *fbapi.ErrorModel, http.Header, error)
	SetProject   func(context.Context, *string) (int, *fbapi.ErrorModel, http.Header, error)
}

// applyGrouping sends only what changed and reports whether the update may
// continue. Tags first, then the project move: the tag write cannot fail for a
// reason the project move would have fixed, and a project move that fails
// leaves the cheaper change already applied rather than losing it. Same
// ordering rule the server resource states for rename/move/resize.
func applyGrouping(ctx context.Context, diags *diag.Diagnostics, u groupingUpdate) bool {
	if u.SetTags != nil && tagsChanged(u.PlanTags, u.StateTags) {
		body := tagsBody(ctx, u.PlanTags, diags)
		if diags.HasError() {
			return false
		}
		status, model, hdr, err := u.SetTags(ctx, body)
		if err != nil {
			apiError(diags, "Setting the "+u.Noun+"'s tags", err)
			return false
		}
		if status >= 400 {
			apiError(diags, "Setting the "+u.Noun+"'s tags", problem(status, model, hdr))
			return false
		}
	}
	if u.SetProject != nil && !u.PlanProject.Equal(u.StateProject) {
		var pid *string
		if v := u.PlanProject.ValueString(); v != "" {
			pid = &v
		}
		status, model, hdr, err := u.SetProject(ctx, pid)
		if err != nil {
			apiError(diags, "Moving the "+u.Noun+" between projects", err)
			return false
		}
		if status >= 400 {
			apiError(diags, "Moving the "+u.Noun+" between projects", problem(status, model, hdr))
			return false
		}
	}
	return true
}

// projectAttribute is the `project_id` attribute on a groupable resource.
//
// In place, never a replacement. Every one of these used to say the API had no
// endpoint to move it and carried RequiresReplace, which on a volume meant
// destroying a disk and its data to change an organizational label. The
// endpoints exist now (`PATCH .../project`, 2026-08-27) and the plan modifier
// came off with them.
func projectAttribute(noun string) rschema.StringAttribute {
	return rschema.StringAttribute{
		Optional: true,
		MarkdownDescription: "Optional project to group the " + noun + " under. " +
			"Changing it moves the " + noun + " in place; it is not a replacement.",
	}
}
