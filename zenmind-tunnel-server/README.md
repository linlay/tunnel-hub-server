# Tunnel Hub Server

Self-hosted HTTP/WebSocket reverse tunnel with a public relay and a local agent. Managed services default to `*.tunnel-hub.zenmind.cc`.

## Commands

- `cmd/relay`: public relay, tunnel endpoint, admin API, and optional admin static file server.
- `cmd/agent`: local process that connects outward to the relay and forwards traffic to local services.

## Relay Environment

| Name | Default | Purpose |
| --- | --- | --- |
| `RELAY_ADDR` | `:8080` | Relay listen address behind the TLS-terminating reverse proxy. |
| `RELAY_DB_PATH` | `zenmind-tunnel.db` | SQLite database path. |
| `ADMIN_HOST` | empty | Admin hostname, for example `admin.example.com`. |
| `WEBSITE_DIST` | empty | Optional built website directory to serve on `ADMIN_HOST`. |
| `PUBLIC_BASE_DOMAIN` | `tunnel-hub.zenmind.cc` | Base domain used by `/api/admin/services/{name}`. |
| `COOKIE_SECRET` | random dev value | HMAC secret for admin sessions. |
| `BOOTSTRAP_ADMIN_USERNAME` | `admin` | First admin username. |
| `BOOTSTRAP_ADMIN_PASSWORD` | `admin` | First admin password. |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | Maximum buffered HTTP request body. |

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
curl -X PUT https://admin.tunnel-hub.zenmind.cc/api/admin/services/auditor \
  -H "Authorization: Bearer $TUNNEL_HUB_ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:3000","active":true}'
```

This creates or updates `auditor.tunnel-hub.zenmind.cc`. Service names must be one lowercase DNS label and cannot be `admin`, `api`, `www`, `tunnel`, or `relay`.
