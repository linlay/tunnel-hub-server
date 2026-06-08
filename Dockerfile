FROM node:25-bookworm AS website

WORKDIR /src/zenmind-tunnel-website
COPY zenmind-tunnel-website/package*.json ./
RUN npm install
COPY zenmind-tunnel-website/ ./
RUN npm run build

FROM golang:1.26-bookworm AS server

WORKDIR /src/zenmind-tunnel-server
COPY zenmind-tunnel-server/go.mod zenmind-tunnel-server/go.sum* ./
RUN go mod download
COPY zenmind-tunnel-server/ ./
RUN CGO_ENABLED=0 go build -o /out/relay ./cmd/relay
RUN CGO_ENABLED=0 go build -o /out/agent ./cmd/agent

FROM gcr.io/distroless/base-debian12

WORKDIR /app
COPY --from=server /out/relay /app/relay
COPY --from=server /out/agent /app/agent
COPY --from=website /src/zenmind-tunnel-website/dist /app/website

ENV RELAY_ADDR=:8080
ENV WEBSITE_DIST=/app/website

ENTRYPOINT ["/app/relay"]

