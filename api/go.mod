module github.com/rlsvarinskis/cloud-at-home/api

go 1.26.0

require (
	connectrpc.com/connect v1.21.1
	google.golang.org/protobuf v1.36.12
)

replace github.com/rlsvarinskis/cloud-at-home/libs/common => ../libs/common

replace github.com/rlsvarinskis/cloud-at-home/libs/authz => ../libs/authz

replace github.com/rlsvarinskis/cloud-at-home/libs/qemu => ../libs/qemu

replace github.com/rlsvarinskis/cloud-at-home/libs/server => ../libs/server

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../services/vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../services/agentd

replace github.com/rlsvarinskis/cloud-at-home/services/mcp => ../services/mcp

replace github.com/rlsvarinskis/cloud-at-home/cmd/hcctl => ../cmd/hcctl

replace github.com/rlsvarinskis/cloud-at-home/e2e => ../e2e
