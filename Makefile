.PHONY: build test test-client test-race run proto licenses

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

licenses:
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	GOBIN="$$tmp" go install github.com/google/go-licenses/v2@v2.0.1; \
	GOROOT=$$(go env GOROOT) "$$tmp/go-licenses" save ./cmd/server \
		--save_path="$$tmp/out" --ignore inno-live-server; \
	count=$$(find "$$tmp/out" -type f | wc -l | tr -d '[:space:]'); \
	if [ "$$count" -eq 0 ]; then \
		echo "licenses: generated 0 files" >&2; \
		exit 1; \
	fi; \
	rm -rf third_party/licenses; \
	mkdir -p third_party; \
	cp -R "$$tmp/out" third_party/licenses
