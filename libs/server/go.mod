module github.com/rlsvarinskis/cloud-at-home/libs/server

go 1.26.0

require github.com/rlsvarinskis/cloud-at-home/api v0.0.0

require github.com/rlsvarinskis/cloud-at-home/libs/common v0.0.0

require (
	connectrpc.com/connect v1.21.1
	github.com/prometheus/client_golang v1.25.0
	github.com/rlsvarinskis/cloud-at-home/libs/authz v0.0.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/ulid/v2 v2.1.2 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/rlsvarinskis/cloud-at-home/api => ../../api

replace github.com/rlsvarinskis/cloud-at-home/libs/common => ../common

replace github.com/rlsvarinskis/cloud-at-home/libs/authz => ../authz

replace github.com/rlsvarinskis/cloud-at-home/libs/qemu => ../qemu

replace github.com/rlsvarinskis/cloud-at-home/services/vmd => ../../services/vmd

replace github.com/rlsvarinskis/cloud-at-home/services/agentd => ../../services/agentd

replace github.com/rlsvarinskis/cloud-at-home/cmd/hcctl => ../../cmd/hcctl

replace github.com/rlsvarinskis/cloud-at-home/e2e => ../../e2e
