ARG GO_IMAGE=golang:1.25-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/base-debian12

FROM ${GO_IMAGE} AS build

ARG GO_MODULE_PATH
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOPROXY=${GOPROXY}

WORKDIR /src
COPY go.mod go.sum* ./
COPY third_party/yamux ./third_party/yamux
RUN go mod download
COPY . .
RUN test -n "$GO_MODULE_PATH" \
    && go run ./tools/moduleprep rewrite -root /src -module "$GO_MODULE_PATH"
RUN CGO_ENABLED=0 go build -o /out/relay ./cmd/relay
RUN CGO_ENABLED=0 go build -o /out/agent ./cmd/agent

FROM ${RUNTIME_IMAGE}

WORKDIR /app
COPY --from=build /out/relay /app/relay
COPY --from=build /out/agent /app/agent

ENTRYPOINT ["/app/relay"]
