# terraform-provider-firstboot

Terraform provider for the Firstboot cloud platform.

```hcl
terraform {
  required_providers {
    firstboot = {
      source = "firstboot-io/firstboot"
    }
  }
}

provider "firstboot" {
  # or FIRSTBOOT_API_URL / FIRSTBOOT_TOKEN
  endpoint = "https://api.example.com"
  token    = var.firstboot_token
}

data "firstboot_images" "all" {}

resource "firstboot_ssh_key" "deploy" {
  name       = "deploy"
  public_key = file("~/.ssh/id_ed25519.pub")
}

resource "firstboot_server" "web" {
  count       = 3
  name        = "web-${count.index + 1}"
  plan        = "s1"
  image       = "ubuntu-24-04"
  region      = "ist"
  ssh_key_ids = [firstboot_ssh_key.deploy.id]
}
```

## Status

Implemented and covered by tests:

| Kind | Name |
|---|---|
| Resource | `firstboot_server` |
| Resource | `firstboot_volume` |
| Resource | `firstboot_network` |
| Resource | `firstboot_firewall` |
| Resource | `firstboot_floating_ip` |
| Resource | `firstboot_dns_zone` |
| Resource | `firstboot_dns_record` |
| Resource | `firstboot_ssh_key` |
| Resource | `firstboot_project` |
| Resource | `firstboot_load_balancer` |
| Resource | `firstboot_database` |
| Resource | `firstboot_app` |
| Resource | `firstboot_iso` |
| Resource | `firstboot_domain` |
| Resource | `firstboot_rdns` |
| Data source | `firstboot_plans` |
| Data source | `firstboot_regions` |
| Data source | `firstboot_images` |
| Data source | `firstboot_servers` |
| Data source | `firstboot_volumes` |
| Data source | `firstboot_networks` |
| Data source | `firstboot_databases` |
| Data source | `firstboot_load_balancers` |
| Data source | `firstboot_dns_zones` |
| Data source | `firstboot_apps` |
| Data source | `firstboot_domains` |

The eight plural data sources select by tag or by project and answer with `ids`
and `names`. They are why the eight resources carry `tags`: a fleet built with
`count` should be handed to a load balancer by role rather than enumerated.

```hcl
resource "firstboot_server" "web" {
  count = 3
  name  = "web-${count.index + 1}"
  plan  = "s1"
  image = "ubuntu-24-04"
  tags  = ["env:prod", "role:web"]
}

data "firstboot_servers" "web" {
  tags = ["role:web"]
}

resource "firstboot_load_balancer" "lb" {
  name        = "web-lb"
  network_id  = firstboot_network.vpc.id
  backend_ids = data.firstboot_servers.web.ids
}
```

Tags must be written in stored form: lowercase, `[a-z0-9._:-]`, starting with a
letter or a digit, at most ten of at most thirty-two characters. `Env:Prod` is
refused at PLAN time rather than quietly rewritten, because the stored value
would be `env:prod` and your configuration and state would then differ forever.

`tags` and `project_id` are both applied IN PLACE on all eight. Neither may ever
grow a `RequiresReplace`: four of them had one, back when the API had no
endpoint to change a project, and it meant destroying a volume and its data to
move an organizational label.

A resource is registered only once it is implemented: a registered constructor
that cannot serve panics at apply rather than answering "unsupported resource
type".

Three things inside the API are deliberately not resources here. The databases
and users INSIDE a managed instance return their credentials once, from an
endpoint an API token cannot call, so managing them would mean either failing or
writing passwords into the state file. Mounting an ISO and starting or stopping
an app or a server are ACTIONS, not properties: a plan should not decide to
reboot something that is serving traffic. And a domain's registrant contact has
no resource yet -- create the profile in the panel and pass its id.

## Six things worth knowing before the first apply

**An apply is a purchase.** A month is charged upfront when a server is created,
and the unused part is refunded when it is destroyed early. `terraform destroy`
is a partial refund, not a no-op.

**A retried create cannot buy a second machine.** The provider goes through the
`firstboot-go` SDK, which sets an `Idempotency-Key` and reuses it across its own
retries. A create whose response is lost to a timeout is answered with the
resource the first attempt made.

**Several resources cannot be changed in place, and the schema says so.** A
private network and a DNS zone have no update endpoint at all; a volume is
formatted once at birth and never again, because afterwards nothing can honestly
answer whether the disk holds data. Every such attribute requires replacement, so
the plan tells the truth rather than applying and changing nothing. A test
enforces it.

**Write-only attributes require replacement.** The API never returns a server's
`ssh_key_ids` or `user_data` -- they are consumed once, at first boot -- nor a
load balancer's `restrict_backends`. The provider can neither detect drift in
them nor change them in place, so they force replacement instead of promising
something the apply would not do. That is also why an imported server needs
either those attributes left out of the configuration or an `ignore_changes`
block; both imports warn about it.

**`terraform destroy` cannot un-register a domain.** A registry does not take a
name back and the term is already paid for, so destroying a `firstboot_domain`
makes Terraform FORGET it: the name stays registered and, with `auto_renew` on,
keeps charging the wallet. The destroy says so in a warning, and the name,
term and registrant refuse a change rather than forcing replacement -- replacing
a domain would abandon a paid registration and buy a second one in the same
apply. Guard the ones that matter:

```hcl
resource "firstboot_domain" "example" {
  name       = "example.com"
  years      = 1
  contact_id = var.registrant_id

  lifecycle {
    prevent_destroy = true
  }
}
```

**An app's environment is never read back.** The endpoint that returns decrypted
values writes an audit entry on every call, so refreshing it on each plan would
fill the account's audit log with reads nobody made deliberately. The
consequence is honest and worth knowing: a value changed in the panel is not
detected as drift, and the next apply that touches the app overwrites it with
whatever the configuration says. An imported app has no `env` in state at all,
and the import warns about it.

## Rate limits

A configuration that creates many servers at once will meet the account's create
rate ceiling (10 per hour by default): apply stops partway with
`CREATE_COOLDOWN`, and the provider's diagnostic says so. The ceiling is an
operator-editable quota (`servers.create_rate`), not a fixed limit, so an account
that legitimately builds fleets can have it raised. The provider already honours
the `Retry-After` the API sends, up to its retry budget.

## Development

```
go build ./...
go test ./...
```

The SDK is consumed from the sibling checkout through a `replace` directive
until it is published. The acceptance tests are not written yet: they need a
real account and spend real money, so they are their own decision rather than
something to add quietly.

To try the provider against a local build, use a `dev_overrides` block in
`~/.terraformrc` pointing `firstboot-io/firstboot` at your `GOBIN`. Note that
`terraform init` is skipped under `dev_overrides`; run `plan` and `apply`
directly.

## License

Apache License 2.0. See [LICENSE](LICENSE).
