module github.com/rlsvarinskis/cloud-at-home/libs/common

go 1.26.0

require (
	connectrpc.com/connect v1.21.1
	github.com/fergusstrange/embedded-postgres v1.34.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/oklog/ulid/v2 v2.1.2
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lib/pq v1.10.9 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/rlsvarinskis/cloud-at-home/api => ../../api

replace github.com/rlsvarinskis/cloud-at-home/libs/authz => ../authz

replace github.com/rlsvarinskis/cloud-at-home/libs/qemu => ../qemu

replace github.com/rlsvarinskis/cloud-at-home/libs/server => ../server

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../../services/vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../../services/agentd

replace github.com/rlsvarinskis/cloud-at-home/services/mcp => ../../services/mcp

replace github.com/rlsvarinskis/cloud-at-home/cmd/atticctl => ../../cmd/atticctl

replace github.com/rlsvarinskis/cloud-at-home/e2e => ../../e2e
