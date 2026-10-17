.PHONY: proto build test vet lint cover mocks tidy

PATH := $(PATH):$(HOME)/go/bin

proto:  ## regenerate gen/ from proto/ (needs protoc, protoc-gen-go, protoc-gen-connect-go)
	rm -rf gen && mkdir -p gen
	protoc -I proto -I /usr/include \
	  --go_out=gen --go_opt=paths=source_relative \
	  --connect-go_out=gen --connect-go_opt=paths=source_relative \
	  $$(find proto -name '*.proto')

mocks:  ## regenerate mocks (go:generate directives)
	go generate ./...

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
