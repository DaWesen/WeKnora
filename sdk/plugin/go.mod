module github.com/Tencent/WeKnora/sdk/plugin

// The plugin SDK is an independently versioned module: external plugin
// repositories consume it via `go get github.com/Tencent/WeKnora/sdk/plugin`
// and never import the host module. Keep the host's gRPC dependency versions
// in sync with the ones below so one binary never links two copies.
go 1.25.0

require (
	google.golang.org/grpc v1.81.0
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260427160629-7cedc36a6bc4 // indirect
)
