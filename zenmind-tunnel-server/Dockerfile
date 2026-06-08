FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/relay ./cmd/relay
RUN CGO_ENABLED=0 go build -o /out/agent ./cmd/agent

FROM gcr.io/distroless/base-debian12

WORKDIR /app
COPY --from=build /out/relay /app/relay
COPY --from=build /out/agent /app/agent

ENTRYPOINT ["/app/relay"]

