.PHONY: proto build test vet mocks tidy cover boundaries

PATH := $(PATH):$(HOME)/go/bin
MODULES := api libs/common libs/authz libs/qemu libs/server services/vmd services/agentd services/mcp cmd/hcctl e2e

proto:  ## regenerate api/gen from api/proto (needs protoc, protoc-gen-go, protoc-gen-connect-go)
	cd api && rm -rf gen && mkdir -p gen && \
	protoc -I proto -I /usr/include \
	  --go_out=gen --go_opt=paths=source_relative \
	  --connect-go_out=gen --connect-go_opt=paths=source_relative \
	  $$(find proto -name '*.proto')

MOCKGEN := go run go.uber.org/mock/mockgen@v0.6.0

mocks:  ## regenerate gomock mocks for the interfaces tests need to script
	cd services/vmd    && $(MOCKGEN) -source=store/store.go     -destination=store/mock/mock_store.go          -package=mock
	cd services/vmd    && $(MOCKGEN) -source=service/nodes.go   -destination=service/servicemock/mock_nodes.go -package=servicemock
	cd services/agentd && $(MOCKGEN) -source=store/store.go     -destination=store/mock/mock_store.go          -package=mock
	cd services/agentd && $(MOCKGEN) -source=llm/llm.go         -destination=llm/mock/mock_llm.go              -package=mock
	cd services/agentd && $(MOCKGEN) -source=vmclient/vmclient.go -destination=vmclient/mock/mock_vmclient.go  -package=mock
	cd libs/qemu       && $(MOCKGEN) -source=node/driver.go     -destination=node/mock/mock_driver.go          -package=mock

build:
	@for m in $(MODULES); do (cd $$m && go build ./...) || exit 1; done

vet:
	@for m in $(MODULES); do (cd $$m && go vet ./...) || exit 1; done

test:
	@for m in $(MODULES); do (cd $$m && go test -race -count=1 ./...) || exit 1; done

cover:
	@for m in $(MODULES); do (cd $$m && go test -race -cover ./... | tail -n +1); done

tidy:
	@for m in $(MODULES); do (cd $$m && go mod tidy) || exit 1; done

boundaries:  ## fail if a module imports one it may not (see docs/ARCHITECTURE.md)
	./scripts/check-boundaries.sh
