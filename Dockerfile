FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

RUN cd examples/plugin/cred-concurrency/go && CGO_ENABLED=1 GOOS=linux go build -buildmode=c-shared -o /app/plugins/cred-concurrency.so .

RUN cd examples/plugin/request-logs/go && CGO_ENABLED=1 GOOS=linux go build -buildmode=c-shared -o /app/plugins/request-logs.so .

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY --from=builder /app/plugins /CLIProxyAPI/plugins

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD bash -c 'PORT=$(grep -oE "^[[:space:]]*port:[[:space:]]*[0-9]+" /CLIProxyAPI/config.yaml | grep -oE "[0-9]+" | head -1); PORT=${PORT:-8317}; exec 3<>/dev/tcp/127.0.0.1/$PORT && printf "GET /healthz HTTP/1.0\r\n\r\n" >&3 && head -1 <&3 | grep -q " 200"'

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
