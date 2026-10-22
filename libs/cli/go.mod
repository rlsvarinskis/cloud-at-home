module github.com/rlsvarinskis/cloud-at-home/libs/cli

go 1.26.0

require (
	connectrpc.com/connect v1.21.1
	github.com/rlsvarinskis/cloud-at-home/api v0.0.0
)

require google.golang.org/protobuf v1.36.12 // indirect

replace github.com/rlsvarinskis/cloud-at-home/api => ../../api
