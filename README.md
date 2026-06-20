# Tunnel Hub Server

Self-hosted HTTP/WebSocket tunnel hub with a public relay and outbound Desktop/agent clients. Management, APIs, and the agent relay live at `tunnel-hub.zenmind.cc`; Desktop broker WebSockets use random hosts under `*.m.zenmind.cc`, while device-scoped webapp tunnels use random hosts under `*.wa.zenmind.cc`.

This repository contains only the Go relay/agent server. The React + Vite management website lives in the sibling repository `tunnel-hub-website`.

## Commands

- `cmd/relay`: public relay, tunnel endpoint, admin API, and optional admin static file server.
- `cmd/agent`: local process that connects outward to the relay and forwards traffic to local services. One tunnel token represents one host/agent identity.

## Relay Environment

Copy `.env.example` to `.env` for local development, then replace any placeholders before production use. Go commands load `.env` from the current working directory automatically, while real shell/container environment variables take precedence.

| Name | Default | Purpose |
| --- | --- | --- |
| `RELAY_ADDR` | `:8080` | Relay listen address behind the TLS-terminating reverse proxy. |
| `RELAY_DB_PATH` | `tunnel.db` | SQLite database path. |
| `ADMIN_HOST` | empty | Optional legacy admin hostname for Relay-served static files. Leave empty in the split website/server deployment. |
| `WEBSITE_DIST` | empty | Optional legacy built website directory. Leave empty in the split website/server deployment. |
| `PUBLIC_BASE_DOMAIN` | `tunnel-hub.zenmind.cc` | Base domain used by `/api/admin/services/{name}`. |
| `DESKTOP_PUBLIC_BASE_DOMAIN` | `m.zenmind.cc` | Base domain used for random Desktop broker hosts returned by `/api/desktop/devices/register`. |
| `WEBAPP_PUBLIC_BASE_DOMAIN` | `wa.zenmind.cc` | Base domain used for device-scoped webapp reverse proxy hosts. |
| `ADMIN_USERNAME` | `admin` | Bootstrap username for local management website login. |
| `ADMIN_PASSWORD` | empty | Bootstrap password for local management website login. Leave empty to skip local admin creation. |
| `ADMIN_SESSION_TTL` | `24h` | Local admin login cookie lifetime. |
| `COOKIE_SECURE` | `false` | Whether local admin cookies are HTTPS-only. |
| `SSO_JWT_ISSUER` | empty | Expected issuer for official-site SSO JWTs. Required. |
| `SSO_JWT_PUBLIC_KEY_FILE` | empty | PEM public key file used to verify SSO JWTs. |
| `SSO_JWT_PUBLIC_KEY_PEM` | empty | PEM public key value fallback; supports escaped `\n`. |
| `SSO_JWT_AUDIENCE` | `zenmind-tunnel-hub-server` | Required JWT audience. |
| `MAX_REQUEST_BODY_BYTES` | `67108864` | Maximum buffered HTTP request body. |

Do not commit key material or production secrets. Keep the SSO JWT public key in the ignored project-local file `configs/jwt-public.pem`, then mount `./configs` read-only in production. `SSO_JWT_ISSUER` must exactly match the issuer configured by the official-site server.

Export the public key from the official-site JWT private key:

```bash
mkdir -p configs
openssl pkey -in /path/to/official-sso-private.pem -pubout -out configs/jwt-public.pem
```

The management website can log in with the local admin username/password and uses an HttpOnly `tunnel_hub_session` cookie. Admin API calls also accept an official-site SSO JWT with `role=admin` and `scope` containing `tunnel`.

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

Use an official-site SSO JWT with `role=admin` and `scope` containing `tunnel`, then publish a local service:

```bash
curl -X PUT https://tunnel-hub.zenmind.cc/api/admin/services/auditor \
  -H "Authorization: Bearer $ZENMIND_OFFICIAL_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:3000","tokenId":"token_...","active":true}'
```

This creates or updates `<service>.$PUBLIC_BASE_DOMAIN` and binds it to the selected token's online agent. Service names must be one lowercase DNS label and cannot be `admin`, `api`, `www`, `tunnel`, or `relay`. The production proxy example in this repo no longer routes `*.tunnel-hub.zenmind.cc`; add a routed wildcard server if you continue using the generic service publish API.

## Desktop Device Registration API

Desktop registration must use an official-site SSO JWT with `scope` containing `tunnel`; local admin cookies are not accepted for Desktop registration:

```bash
curl -X POST https://tunnel-hub.zenmind.cc/api/desktop/devices/register \
  -H "Authorization: Bearer $ZENMIND_OFFICIAL_JWT" \
  -H "Content-Type: application/json" \
  -d '{"deviceId":"mac-mini","deviceName":"Frank MacBook Pro","rotateToken":false}'
```

The first successful registration creates a tunnel token and a random Desktop broker host such as `zmabc123def4.m.zenmind.cc`; the public host never uses `deviceId` and no `routes.target_url` is created for `*.m.zenmind.cc`. Re-registering the same `(user_id, deviceId)` reuses the existing public host and token. Different users may register the same `deviceId` and receive independent random public hosts. Set `rotateToken` to `true` to invalidate the old tunnel token and receive a new `agentToken`. Legacy `deviceSecret` and `targetUrl` request fields are ignored for Desktop registration.

Browsers and testers can then connect to the Desktop remote WebSocket through `wss://<random>.m.zenmind.cc/ws`. The hub opens a `desktop.websocket` tunnel stream to the connected Desktop App and forwards WebSocket metadata and frames transparently; Desktop still validates its own access token before accepting control messages. Only WebSocket requests are accepted on `*.m.zenmind.cc`.

## Desktop WebApp Registration API

Desktop-owned webapps keep the target-port reverse proxy model and are published under `*.wa.zenmind.cc`:

```bash
curl -X PUT https://tunnel-hub.zenmind.cc/api/desktop/devices/mac-mini/webapps/notes \
  -H "Authorization: Bearer $ZENMIND_OFFICIAL_JWT" \
  -H "Content-Type: application/json" \
  -d '{"targetUrl":"http://127.0.0.1:5173","active":true}'
```

This creates or updates a random public host such as `zwaabc123def4.wa.zenmind.cc`, owned by the Desktop device and bound to that Desktop tunnel token. `targetUrl` is required for webapps because the connected Desktop tunnel client dials the configured local service port.

## Public Component List API

`GET /api/components` is public and does not require a JWT. It returns safe component fields such as `publicHost`, `publicUrl`, `active`, and `updatedAt`; it does not expose internal route IDs, `targetUrl`, `tokenId`, or secrets.

## Split Production Deployment

In production, run Relay and the management website as separate services:

- Relay: `127.0.0.1:11961 -> 8080`
- Website: `127.0.0.1:11963 -> 80`
- Public Nginx routes `tunnel-hub.zenmind.cc/` to the website, `tunnel-hub.zenmind.cc/api/admin`, `tunnel-hub.zenmind.cc/api/desktop`, and `/tunnel` to Relay, and all `*.m.zenmind.cc` plus `*.wa.zenmind.cc` traffic directly to Relay.

The example in `deploy/nginx/zenmind-tunnel.conf` matches this split deployment, so business tunnel traffic does not pass through the website container.
