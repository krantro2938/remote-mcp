# Relay image. Also builds agent.exe so the relay can serve it at /download/.
#
#   docker build -t remote-mcp .
#   docker run -d --name remote-mcp --env-file relay.env -p 127.0.0.1:18080:8080 \
#     -v remote-mcp-data:/data remote-mcp
#
# deploy/docker-deploy.sh does all of this (plus secrets and Caddy) on a VPS.
FROM golang:1.24-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay \
 && GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/agent.exe ./cmd/agent \
 && GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /out/agent-arm64.exe ./cmd/agent

FROM alpine:3.22
RUN adduser -D -H -u 10001 remote-mcp \
 && mkdir /data && chown remote-mcp:remote-mcp /data
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/relay /usr/local/bin/remote-mcp-relay
COPY --from=build /out/agent.exe /out/agent-arm64.exe /opt/remote-mcp/
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV LISTEN_ADDR=0.0.0.0:8080 DATA_DIR=/data
VOLUME /data
USER remote-mcp
EXPOSE 8080
ENTRYPOINT ["docker-entrypoint.sh"]
