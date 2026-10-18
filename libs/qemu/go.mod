module github.com/rlsvarinskis/cloud-at-home/libs/qemu

go 1.26.0

require (
	connectrpc.com/connect v1.21.1
	github.com/rlsvarinskis/cloud-at-home/api v0.0.0
	github.com/rlsvarinskis/cloud-at-home/libs/authz v0.0.0
	github.com/rlsvarinskis/cloud-at-home/libs/common v0.0.0
	go.uber.org/mock v0.6.0
	golang.org/x/crypto v0.58.0
)

require (
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	golang.org/x/sys v0.49.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/rlsvarinskis/cloud-at-home/api => ../../api

replace github.com/rlsvarinskis/cloud-at-home/libs/common => ../common

replace github.com/rlsvarinskis/cloud-at-home/libs/authz => ../authz

replace github.com/rlsvarinskis/cloud-at-home/libs/server => ../server

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../../services/vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../../services/agentd

replace github.com/rlsvarinskis/cloud-at-home/cmd/hcctl => ../../cmd/hcctl

replace github.com/rlsvarinskis/cloud-at-home/e2e => ../../e2e
