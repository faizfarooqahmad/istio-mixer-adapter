module github.com/apigee/istio-mixer-adapter/grpc-server

go 1.13

replace github.com/apigee/istio-mixer-adapter/apigee => ../apigee

replace github.com/apigee/istio-mixer-adapter/mixer => ../mixer

require (
	github.com/apigee/istio-mixer-adapter/mixer v0.0.0-00010101000000-000000000000 // indirect
	github.com/spf13/cobra v0.0.6 // indirect
)