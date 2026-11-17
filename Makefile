export CGO_ENABLED=0
LDFLAGS := -s -w

.PHONY: all relay agent test vet clean demo

all: relay agent

relay:
	go build -trimpath -ldflags="$(LDFLAGS)" -o dist/relay ./cmd/relay

# The Windows agent cross-compiles from any OS (no CGO).
agent:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/agent.exe ./cmd/agent
	GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/agent-arm64.exe ./cmd/agent

test:
	go test -race ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

clean:
	rm -rf dist

# Local end-to-end playground: relay on :8080 plus an agent with a fake desktop.
demo: relay
	go build -o dist/agent-local ./cmd/agent
	PUBLIC_URL=http://localhost:8080 LISTEN_ADDR=127.0.0.1:8080 DATA_DIR=./dist/data \
	AGENT_TOKENS=demo=demo-token-0123456789abcdef ADMIN_PASSWORD=demo-password \
	MCP_TOKEN=mcp-token-0123456789abcdef ./dist/relay & \
	sleep 1; ./dist/agent-local -demo -server http://localhost:8080 -token demo-token-0123456789abcdef -name demo
