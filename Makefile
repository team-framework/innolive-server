.PHONY: build test test-race run proto

build:
	go build ./cmd/server

test:
	go test ./...

test-race:
	go test -race ./...

run:
	go run ./cmd/server

proto:
	protoc --go_out=. --go_opt=module=inno-live-server \
		--go-grpc_out=. --go-grpc_opt=module=inno-live-server \
		api/proto/ai_processor.proto
