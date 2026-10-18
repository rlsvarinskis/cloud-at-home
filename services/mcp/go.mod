module github.com/rlsvarinskis/cloud-at-home/services/mcp

go 1.26.0

require (
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

require github.com/rlsvarinskis/cloud-at-home/api v0.0.0

require github.com/rlsvarinskis/cloud-at-home/libs/authz v0.0.0

require (
	connectrpc.com/connect v1.21.1
	github.com/rlsvarinskis/cloud-at-home/libs/common v0.0.0 // indirect
)

replace github.com/rlsvarinskis/cloud-at-home/api => ../../api

replace github.com/rlsvarinskis/cloud-at-home/libs/common => ../../libs/common

replace github.com/rlsvarinskis/cloud-at-home/libs/authz => ../../libs/authz

replace github.com/rlsvarinskis/cloud-at-home/libs/qemu => ../../libs/qemu

replace github.com/rlsvarinskis/cloud-at-home/libs/server => ../../libs/server

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../agentd

replace github.com/rlsvarinskis/cloud-at-home/cmd/hcctl => ../../cmd/hcctl

replace github.com/rlsvarinskis/cloud-at-home/e2e => ../../e2e
