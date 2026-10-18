module github.com/rlsvarinskis/cloud-at-home/e2e

go 1.26.0

require (
	connectrpc.com/connect v1.21.1
	github.com/jackc/pgx/v5 v5.11.0
	github.com/prometheus/client_golang v1.25.0
	github.com/rlsvarinskis/cloud-at-home/api v0.0.0
	github.com/rlsvarinskis/cloud-at-home/libs/authz v0.0.0
	github.com/rlsvarinskis/cloud-at-home/libs/common v0.0.0
	github.com/rlsvarinskis/cloud-at-home/libs/qemu v0.0.0
	github.com/rlsvarinskis/cloud-at-home/services/agentd v0.0.0
	github.com/rlsvarinskis/cloud-at-home/services/vmd v0.0.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/fergusstrange/embedded-postgres v1.34.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

require github.com/rlsvarinskis/cloud-at-home/libs/server v0.0.0

replace github.com/rlsvarinskis/cloud-at-home/api => ../api

replace github.com/rlsvarinskis/cloud-at-home/libs/common => ../libs/common

replace github.com/rlsvarinskis/cloud-at-home/libs/authz => ../libs/authz

replace github.com/rlsvarinskis/cloud-at-home/libs/qemu => ../libs/qemu

replace github.com/rlsvarinskis/cloud-at-home/libs/server => ../libs/server

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../services/vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../services/agentd

replace github.com/rlsvarinskis/cloud-at-home/services/mcp => ../services/mcp

replace github.com/rlsvarinskis/cloud-at-home/cmd/atticctl => ../cmd/atticctl

require (
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/rlsvarinskis/cloud-at-home/services/mcp v0.0.0
)
