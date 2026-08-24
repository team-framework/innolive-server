.PHONY: build test test-client test-race run proto

build:
	go build ./cmd/server

test:
	go test ./...
	node --test internal/server/static/client/app.test.mjs

test-client:
	node --test internal/server/static/client/app.test.mjs

test-race:
	go test -race ./...

run:
	go run ./cmd/server

proto:
	protoc --go_out=. --go_opt=module=inno-live-server \
		--go-grpc_out=. --go-grpc_opt=module=inno-live-server \
		api/proto/ai_processor.proto
