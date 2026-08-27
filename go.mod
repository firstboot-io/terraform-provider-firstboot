module github.com/firstboot-io/terraform-provider-firstboot

go 1.25.7

// firstboot-go is not published yet; it is checked out as a sibling of this
// repository, exactly as the workspace layout describes. Remove this the day
// the SDK carries a tag.
replace github.com/firstboot-io/firstboot-go => ../go-sdk

require (
	github.com/firstboot-io/firstboot-go v0.0.0-00010101000000-000000000000
	github.com/google/uuid v1.6.0
	github.com/hashicorp/terraform-plugin-framework v1.19.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.7.0 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/hashicorp/terraform-plugin-go v0.31.0 // indirect
	github.com/hashicorp/terraform-plugin-log v0.10.0 // indirect
	github.com/hashicorp/terraform-registry-address v0.4.0 // indirect
	github.com/hashicorp/terraform-svchost v0.1.1 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mitchellh/go-testing-interface v1.14.1 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/grpc v1.79.2 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
