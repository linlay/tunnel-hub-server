# Tunnel Hub Server

Self-hosted HTTP/WebSocket reverse tunnel with a public relay and local agents. Managed services default to `*.tunnel-hub.zenmind.cc`.

This repository contains only the Go relay/agent server. The React + Vite management website lives in the sibling repository `tunnel-hub-website`.

## Commands

- `cmd/relay`: public relay, tunnel endpoint, admin API, and optional admin static file server.
- `cmd/agent`: local process that connects outward to the relay and forwards traffic to local services. One tunnel token represents one host/agent identity.

## Relay Environment

Copy `.env.example` to `.env` for local development, then replace any placeholders before production use. Go commands load `.env` from the current working directory automatically, while real shell/container environment variables take precedence.

| Name | Default | Purpose |
| --- | --- | --- |
| `RELAY_ADDR` | `:8080` | Relay listen address behind the TLS-terminating reverse proxy. |
| `RELAY_DB_PATH` | `zenmind-tunnel.db` | SQLite database path. |
| `ADMIN_HOST` | empty | Optional legacy admin hostname for Relay-served static files. Leave empty in the split website/server deployment. |
| `WEBSITE_DIST` | empty | Optional legacy built website directory. Leave empty in the split website/server deployment. |
| `PUBLIC_BASE_DOMAIN` | `tunnel-hub.zenmind.cc` | Base domain used by `/api/admin/services/{name}`. |
| `COOKIE_SECRET` | random dev value | HMAC secret for admin sessions. |
| `BOOTSTRAP_ADMIN_USERNAME` | `admin` | First admin username. |
| `BOOTSTRAP_ADMIN_PASSWORD` | `admin` | First admin password. |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | Maximum buffered HTTP request body. |

Do not commit real production secrets. `COOKIE_SECRET` must be a long, stable random value in production so admin sessions remain valid across restarts.

## Agent Environment

| Name | Default | Purpose |
| --- | --- | --- |
| `AGENT_RELAY_URL` | `ws://127.0.0.1:8080/tunnel` | Relay tunnel endpoint. Use `wss://.../tunnel` in production. |
| `AGENT_TOKEN` | required | Token created from the admin UI/API. |
| `AGENT_TLS_INSECURE_SKIP_VERIFY` | `false` | Development-only TLS bypass. |
| `AGENT_RECONNECT_SECONDS` | `3` | Reconnect delay. |

## Development

```bash
go test ./...
go run ./cmd/relay
AGENT_TOKEN=<token> AGENT_RELAY_URL=ws://127.0.0.1:8080/tunnel go run ./cmd/agent
```

## Managed Service Publish API

Create an Admin API Key from the console or `/api/admin/api-keys`, then publish a local service:

```bash
curl -X PUT https://tunnel-hub.zenmind.cc/api/admin/services/auditor \
  -H "Authorization: Bearer $TUNNEL_HUB_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:3000","tokenId":"token_...","active":true}'
```

This creates or updates `auditor.tunnel-hub.zenmind.cc` and binds it to the selected token's online agent. Service names must be one lowercase DNS label and cannot be `admin`, `api`, `www`, `tunnel`, or `relay`.

## Split Production Deployment

In production, run Relay and the management website as separate services:

- Relay: `127.0.0.1:11961 -> 8080`
- Website: `127.0.0.1:11963 -> 80`
- Public Nginx routes `tunnel-hub.zenmind.cc/` to the website, `tunnel-hub.zenmind.cc/api/admin` and `/tunnel` to Relay, and all `*.tunnel-hub.zenmind.cc` traffic directly to Relay.

The example in `deploy/nginx/zenmind-tunnel.conf` matches this split deployment, so business tunnel traffic does not pass through the website container.
