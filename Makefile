.PHONY: proto build test vet lint cover mocks tidy

PATH := $(PATH):$(HOME)/go/bin

proto:  ## regenerate gen/ from proto/ (needs protoc, protoc-gen-go, protoc-gen-connect-go)
	rm -rf gen && mkdir -p gen
	protoc -I proto -I /usr/include \
	  --go_out=gen --go_opt=paths=source_relative \
	  --connect-go_out=gen --connect-go_opt=paths=source_relative \
	  $$(find proto -name '*.proto')

MOCKGEN := go run go.uber.org/mock/mockgen@v0.6.0

mocks:  ## regenerate gomock mocks for the interfaces tests need to script
	$(MOCKGEN) -source=internal/vmd/store/store.go        -destination=internal/vmd/store/mock/mock_store.go          -package=mock
	$(MOCKGEN) -source=internal/agentd/store/store.go     -destination=internal/agentd/store/mock/mock_store.go       -package=mock
	$(MOCKGEN) -source=internal/vmd/node/driver.go        -destination=internal/vmd/node/mock/mock_driver.go          -package=mock
	$(MOCKGEN) -source=internal/vmd/service/nodes.go      -destination=internal/vmd/service/servicemock/mock_nodes.go -package=servicemock
	$(MOCKGEN) -source=internal/agentd/llm/llm.go         -destination=internal/agentd/llm/mock/mock_llm.go           -package=mock
	$(MOCKGEN) -source=internal/agentd/vmclient/vmclient.go -destination=internal/agentd/vmclient/mock/mock_vmclient.go -package=mock

build:
	go build ./...

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

cover:
	go test -race -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1

tidy:
	go mod tidy
