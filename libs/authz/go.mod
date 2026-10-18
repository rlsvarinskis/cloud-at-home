module github.com/rlsvarinskis/cloud-at-home/libs/authz

go 1.26.0

require (
	connectrpc.com/connect v1.21.1
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/rlsvarinskis/cloud-at-home/api v0.0.0
	github.com/rlsvarinskis/cloud-at-home/libs/common v0.0.0
)

require (
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/rlsvarinskis/cloud-at-home/api => ../../api

replace github.com/rlsvarinskis/cloud-at-home/libs/common => ../common

replace github.com/rlsvarinskis/cloud-at-home/libs/qemu => ../qemu

replace github.com/rlsvarinskis/cloud-at-home/libs/server => ../server

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../../services/vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../../services/agentd

replace github.com/rlsvarinskis/cloud-at-home/cmd/hcctl => ../../cmd/hcctl

replace github.com/rlsvarinskis/cloud-at-home/e2e => ../../e2e
