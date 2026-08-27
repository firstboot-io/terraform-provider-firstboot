package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Schema-level tests, deliberately without a live API.
//
// Acceptance tests need a real account, real money and a `TF_ACC` opt-in, so
// they cannot run in CI here and are not what catches the mistakes this
// provider is most likely to make. These are: a schema that fails validation, a
// resource registered but not implemented, and -- the one that matters most --
// a write-only attribute that forgot to require replacement, which produces a
// provider that silently fails to apply a change the plan promised.

func newProvider() fwprovider.Provider { return New("test")() }

// A resource whose schema does not validate fails at `terraform plan` with a
// message about the provider rather than about the configuration, which is the
// hardest kind of bug to report.
func TestSchemasValidate(t *testing.T) {
	ctx := context.Background()
	p := newProvider()

	for _, f := range p.Resources(ctx) {
		r := f()
		if r == nil {
			t.Fatal("a registered resource constructor returned nil; it would panic at apply")
		}
		var meta resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "firstboot"}, &meta)
		var got resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &got)
		if got.Diagnostics.HasError() {
			t.Errorf("%s: schema has errors: %v", meta.TypeName, got.Diagnostics)
		}
		if diags := got.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("%s: %v", meta.TypeName, diags)
		}
	}

	for _, f := range p.DataSources(ctx) {
		d := f()
		if d == nil {
			t.Fatal("a registered data source constructor returned nil; it would panic at read")
		}
		var meta datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "firstboot"}, &meta)
		var got datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &got)
		if got.Diagnostics.HasError() {
			t.Errorf("%s: schema has errors: %v", meta.TypeName, got.Diagnostics)
		}
		if diags := got.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("%s: %v", meta.TypeName, diags)
		}
	}
}

// Every resource must implement import. A resource without it cannot be adopted
// into Terraform, which for infrastructure that already exists is the difference
// between using this provider and not.
func TestEveryResourceCanBeImported(t *testing.T) {
	ctx := context.Background()
	for _, f := range newProvider().Resources(ctx) {
		r := f()
		var meta resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "firstboot"}, &meta)
		if _, ok := r.(resource.ResourceWithImportState); !ok {
			t.Errorf("%s does not implement ImportState", meta.TypeName)
		}
		if _, ok := r.(resource.ResourceWithConfigure); !ok {
			t.Errorf("%s does not implement Configure, so its client is always nil", meta.TypeName)
		}
	}
}

// The one this file exists for.
//
// `ssh_key_ids` and `user_data` are never returned by the API, so the provider
// cannot detect drift in them and cannot apply a change to them either -- the
// values are consumed once, at first boot. Without RequiresReplace, a plan says
// it will change them, the apply succeeds, and the machine is untouched: a lie
// that survives every subsequent plan because state now holds the new value.
func TestWriteOnlyServerAttributesRequireReplacement(t *testing.T) {
	ctx := context.Background()
	var got resource.SchemaResponse
	NewServerResource().Schema(ctx, resource.SchemaRequest{}, &got)

	for _, name := range []string{"ssh_key_ids", "user_data", "image", "region", "network_id", "firewall_id"} {
		attr, ok := got.Schema.Attributes[name]
		if !ok {
			t.Errorf("firstboot_server has no %q attribute", name)
			continue
		}
		if !hasRequiresReplace(attr) {
			t.Errorf("firstboot_server.%s does not require replacement.\n"+
				"  It is either write-only (the API never returns it) or not changeable in "+
				"place, so without this a plan promises a change the apply cannot make.", name)
		}
	}
}

// The same rule for the resources whose API offers no update at all. A network
// or a DNS zone that let a plan promise a rename would apply successfully and
// change nothing, and state would then hold a name the platform never had.
func TestImmutableResourcesRequireReplacement(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		res   func() resource.Resource
		name  string
		attrs []string
	}{
		// Neither has an update endpoint for what it IS, so every attribute
		// describing the thing itself has to force replacement.
		//
		// `project_id` and `tags` came OFF these lists on 2026-08-27, when the
		// grouping endpoints shipped: a change to either is now applied in
		// place. Leaving them here would have kept forcing a replacement that
		// deletes a private network, or a DNS zone and every record in it, to
		// change an organizational label.
		{NewNetworkResource, "firstboot_network", []string{"name", "cidr"}},
		{NewDNSZoneResource, "firstboot_dns_zone", []string{"name"}},
		// A volume has a resize and an attach, so only the write-once halves
		// belong here. fs_type is the sharp one: a volume is formatted at birth
		// and never again, because afterwards nothing can honestly answer
		// whether the disk holds data.
		{NewVolumeResource, "firstboot_volume", []string{"name", "fs_type"}},
		// The record's identity fields: the API's update changes content and
		// ttl, and nothing else.
		{NewDNSRecordResource, "firstboot_dns_record", []string{"zone_id", "name", "type"}},
		// An address belongs to its region and can only be attached within it.
		{NewFloatingIPResource, "firstboot_floating_ip", []string{"region"}},
		// A load balancer has no resize and no rename, and its address is tied
		// to the resource, so a change to any of these is a new address.
		// restrict_backends is the write-only one: the API never returns it, so
		// a change here cannot be detected OR applied.
		{NewLoadBalancerResource, "firstboot_load_balancer",
			[]string{"name", "network_id", "plan", "region", "restrict_backends"}},
		// The instance's identity and its engine. A resize exists, which is why
		// `plan` is NOT in this list; nothing converts postgresql to mysql or
		// moves a major version in place.
		{NewDatabaseResource, "firstboot_database",
			[]string{"name", "engine", "engine_version", "region", "network_id"}},
		// An app's placement. Its name, plan, scale and source all have their
		// own endpoints; these two do not. `project_id` left this list with the
		// grouping endpoints (2026-08-27).
		{NewAppResource, "firstboot_app", []string{"region", "runtime"}},
		// An ISO has a create, a read and a delete and nothing else.
		{NewISOResource, "firstboot_iso", []string{"name", "url", "checksum"}},
		// Which address a PTR is for. The hostname is the only editable half.
		{NewRDNSResource, "firstboot_rdns", []string{"server_id", "address"}},
	} {
		var got resource.SchemaResponse
		c.res().Schema(ctx, resource.SchemaRequest{}, &got)
		for _, name := range c.attrs {
			attr, ok := got.Schema.Attributes[name]
			if !ok {
				t.Errorf("%s has no %q attribute", c.name, name)
				continue
			}
			if !hasRequiresReplace(attr) {
				t.Errorf("%s.%s does not require replacement, but the API cannot change it in place.\n"+
					"  A plan would promise the change, the apply would succeed, and nothing "+
					"would happen.", c.name, name)
			}
		}
	}
}

// hasRequiresReplace reports whether an attribute carries a plan modifier that
// forces replacement.
//
// It reads the modifier's own description rather than its concrete type,
// because the framework's RequiresReplace types are unexported and this
// provider also ships its own (listRequiresReplace). Both wordings are matched:
// the framework says "destroy and recreate", this provider says "replaces",
// and matching only one of them is how this test passed while meaning nothing.
func hasRequiresReplace(attr rschema.Attribute) bool {
	var mods []string
	switch a := attr.(type) {
	case rschema.StringAttribute:
		for _, m := range a.PlanModifiers {
			mods = append(mods, m.Description(context.Background()))
		}
	case rschema.ListAttribute:
		for _, m := range a.PlanModifiers {
			mods = append(mods, m.Description(context.Background()))
		}
	// Bool and Int64 are here because write-only attributes are not all
	// strings: `restrict_backends` is a bool the API never returns, and a
	// switch that only knew about strings would have reported it as fine.
	case rschema.BoolAttribute:
		for _, m := range a.PlanModifiers {
			mods = append(mods, m.Description(context.Background()))
		}
	case rschema.Int64Attribute:
		for _, m := range a.PlanModifiers {
			mods = append(mods, m.Description(context.Background()))
		}
	}
	for _, d := range mods {
		d = strings.ToLower(d)
		if strings.Contains(d, "replace") || strings.Contains(d, "recreate") {
			return true
		}
	}
	return false
}

// A domain is the one resource where RequiresReplace would be WRONG.
//
// Replacement means destroy-then-create, and a domain's destroy cannot withdraw
// a registration -- it only makes Terraform forget one that is still paid for
// and still renewing. So a plan that "replaces" a domain on an edited name would
// abandon a name and buy another one in the same apply, from a typo.
//
// The identity attributes therefore REFUSE a change instead. This test holds
// both halves: that they refuse, and that none of them quietly went back to
// forcing replacement.
func TestDomainIdentityRefusesChangeRatherThanReplacing(t *testing.T) {
	ctx := context.Background()
	var got resource.SchemaResponse
	NewDomainResource().Schema(ctx, resource.SchemaRequest{}, &got)

	// `project_id` is deliberately NOT in this list any more. It was, and the
	// reason was true: there was no endpoint that moved a domain between
	// projects, so a change could neither be applied nor safely replaced. The
	// endpoint exists since 2026-08-27, which leaves only the three attributes
	// that are immutable because the REGISTRATION is: a name that was bought,
	// the term it was bought for, and the registrant it was bought in the name
	// of.
	for _, name := range []string{"name", "years", "contact_id"} {
		attr, ok := got.Schema.Attributes[name]
		if !ok {
			t.Errorf("firstboot_domain has no %q attribute", name)
			continue
		}
		if !refusesChange(attr) {
			t.Errorf("firstboot_domain.%s does not refuse a change.\n"+
				"  It cannot be applied and it must not be replaced either: replacing a domain "+
				"abandons a paid registration and buys a second one.", name)
		}
		if hasRequiresReplace(attr) {
			t.Errorf("firstboot_domain.%s forces replacement.\n"+
				"  Destroying a domain does not withdraw the registration, so replacement would "+
				"leave the old name registered, renewing and unmanaged.", name)
		}
	}
}

// refusesChange reports whether an attribute carries one of this provider's
// refusing plan modifiers. It matches on the TYPE rather than on the wording,
// because the wording is per-attribute and a description match would pass on
// any modifier that happened to mention the word.
func refusesChange(attr rschema.Attribute) bool {
	switch a := attr.(type) {
	case rschema.StringAttribute:
		for _, m := range a.PlanModifiers {
			if _, ok := m.(immutableString); ok {
				return true
			}
		}
	case rschema.Int64Attribute:
		for _, m := range a.PlanModifiers {
			if _, ok := m.(immutableInt64); ok {
				return true
			}
		}
	}
	return false
}

// The provider's own schema has to name both configuration knobs, and the token
// has to be marked sensitive or it lands in plan output and in CI logs.
func TestProviderSchema(t *testing.T) {
	ctx := context.Background()
	var got fwprovider.SchemaResponse
	newProvider().Schema(ctx, fwprovider.SchemaRequest{}, &got)
	if got.Diagnostics.HasError() {
		t.Fatal(got.Diagnostics)
	}
	tok, ok := got.Schema.Attributes["token"]
	if !ok {
		t.Fatal("the provider has no token attribute")
	}
	if !tok.IsSensitive() {
		t.Error("the provider's token is not marked sensitive; it would be printed in plan output")
	}
	if _, ok := got.Schema.Attributes["endpoint"]; !ok {
		t.Error("the provider has no endpoint attribute")
	}
}

// Configure must not panic on the framework's own nil-ProviderData walk, which
// it performs before the provider is configured. A resource that assumes a
// client is present crashes there, before any diagnostic can be shown.
func TestConfigureToleratesNilProviderData(t *testing.T) {
	ctx := context.Background()
	for _, f := range newProvider().Resources(ctx) {
		r, ok := f().(resource.ResourceWithConfigure)
		if !ok {
			continue
		}
		var resp resource.ConfigureResponse
		r.Configure(ctx, resource.ConfigureRequest{ProviderData: nil}, &resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("Configure errored on nil ProviderData: %v", resp.Diagnostics)
		}
	}
}

var _ = dsschema.Schema{}

// Every groupable resource has both axes, and both are in-place.
//
// This is the mirror of what TestImmutableResourcesRequireReplacement stopped
// asserting on 2026-08-27. `project_id` used to force replacement on four
// resources and refuse a change on a fifth, each with a comment saying the API
// had no endpoint for it. The endpoints exist now, and the danger runs the
// other way: a `RequiresReplace` reintroduced here would destroy a volume and
// its data, or a DNS zone and its records, to change an organizational label.
func TestGroupableResourcesCarryBothAxesInPlace(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		res  func() resource.Resource
		name string
	}{
		{NewServerResource, "firstboot_server"},
		{NewVolumeResource, "firstboot_volume"},
		{NewNetworkResource, "firstboot_network"},
		{NewDatabaseResource, "firstboot_database"},
		{NewLoadBalancerResource, "firstboot_load_balancer"},
		{NewDNSZoneResource, "firstboot_dns_zone"},
		{NewAppResource, "firstboot_app"},
		{NewDomainResource, "firstboot_domain"},
	} {
		var got resource.SchemaResponse
		c.res().Schema(ctx, resource.SchemaRequest{}, &got)
		for _, name := range []string{"tags", "project_id"} {
			attr, ok := got.Schema.Attributes[name]
			if !ok {
				t.Errorf("%s has no %q attribute, but the API can group it", c.name, name)
				continue
			}
			if hasRequiresReplace(attr) {
				t.Errorf("%s.%s forces replacement.\n"+
					"  Both grouping axes are applied in place (PUT .../tags, PATCH .../project). "+
					"Replacing a resource to change a label destroys it.", c.name, name)
			}
			if refusesChange(attr) {
				t.Errorf("%s.%s refuses a change.\n"+
					"  The endpoint exists; refusing makes a plan block something the apply can do.",
					c.name, name)
			}
		}
	}
}

// A tag is refused at PLAN time when it is not already in stored form.
//
// The API lowercases what it stores, so `Env:Prod` would come back `env:prod`
// and the configuration would disagree with state on every subsequent plan --
// a diff nothing can resolve. Refusing is the only honest answer; rewriting the
// value silently would be the other one, and it is worse.
func TestTagValidatorRefusesWhatWouldNeverSettle(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		tags []string
		ok   bool
	}{
		{"lowercase", []string{"env:prod", "role:web"}, true},
		{"dotted and dashed", []string{"team.billing", "tier-1"}, true},
		{"uppercase", []string{"Env:Prod"}, false},
		{"space", []string{"env prod"}, false},
		{"leading dash", []string{"-env"}, false},
		{"too long", []string{strings.Repeat("a", 33)}, false},
		{"eleven", []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "b1", "b2"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, diags := types.SetValueFrom(ctx, types.StringType, c.tags)
			if diags.HasError() {
				t.Fatalf("building the set: %v", diags)
			}
			var resp validator.SetResponse
			tagSetValidator{}.ValidateSet(ctx, validator.SetRequest{
				Path: path.Root("tags"), ConfigValue: set,
			}, &resp)
			if got := !resp.Diagnostics.HasError(); got != c.ok {
				t.Fatalf("accepted=%v, want %v (%v)", got, c.ok, resp.Diagnostics)
			}
		})
	}
}

// Every groupable kind has a plural data source, and every one of them takes
// both filters. Seven of eight would make the eighth look unsupported, and the
// one that gets forgotten is whichever kind was added last.
func TestEveryGroupableKindHasASelector(t *testing.T) {
	ctx := context.Background()
	want := map[string]bool{
		"firstboot_servers": false, "firstboot_volumes": false,
		"firstboot_networks": false, "firstboot_databases": false,
		"firstboot_load_balancers": false, "firstboot_dns_zones": false,
		"firstboot_apps": false, "firstboot_domains": false,
	}
	for _, mk := range GroupedDataSources() {
		ds := mk()
		var meta datasource.MetadataResponse
		ds.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "firstboot"}, &meta)
		if _, ok := want[meta.TypeName]; !ok {
			t.Errorf("%s is a selector for a kind that is not groupable", meta.TypeName)
			continue
		}
		want[meta.TypeName] = true

		var got datasource.SchemaResponse
		ds.Schema(ctx, datasource.SchemaRequest{}, &got)
		for _, attr := range []string{"tags", "project_id", "ids", "names"} {
			if _, ok := got.Schema.Attributes[attr]; !ok {
				t.Errorf("%s has no %q attribute", meta.TypeName, attr)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s is missing: the kind can be tagged but not selected", name)
		}
	}
}
