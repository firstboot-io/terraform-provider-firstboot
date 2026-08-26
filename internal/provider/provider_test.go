package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
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
		// No update endpoint exists for either, so every configurable
		// attribute has to force replacement.
		{NewNetworkResource, "firstboot_network", []string{"name", "cidr", "project_id"}},
		{NewDNSZoneResource, "firstboot_dns_zone", []string{"name", "project_id"}},
		// A volume has a resize and an attach, so only the write-once halves
		// belong here. fs_type is the sharp one: a volume is formatted at birth
		// and never again, because afterwards nothing can honestly answer
		// whether the disk holds data.
		{NewVolumeResource, "firstboot_volume", []string{"name", "fs_type", "project_id"}},
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
		// own endpoints; these three do not.
		{NewAppResource, "firstboot_app", []string{"region", "project_id", "runtime"}},
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

	for _, name := range []string{"name", "years", "contact_id", "project_id"} {
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
