module github.com/yourname/weknora-plugin-local-files

go 1.25.0

require (
	github.com/Tencent/WeKnora/sdk/plugin v0.1.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260427160629-7cedc36a6bc4 // indirect
	google.golang.org/grpc v1.81.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Development-only: resolve the SDK from a local checkout until
// github.com/Tencent/WeKnora pushes the sdk/plugin/v0.1.0 submodule tag.
// Delete this block and run `go mod tidy` once the tag is published —
// `go get github.com/Tencent/WeKnora/sdk/plugin` then works as-is.
replace github.com/Tencent/WeKnora/sdk/plugin => ../../../sdk/plugin
