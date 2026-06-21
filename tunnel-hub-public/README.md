# Tunnel Hub Public

## Overview

`tunnel-hub-public` is the lightweight browser client for ZenMind Desktop public hosts such as `https://zm...m.zenmind.cc/`.

Normal HTTP requests render this responsive mini site. WebSocket upgrades still use `wss://<host>/ws` and are routed by the host reverse proxy to `tunnel-hub-server`.

## Development

```bash
cd tunnel-hub-public
npm install
npm test
npm run build
npm run dev
```

Open `http://127.0.0.1:11965`.

## Auth

The page accepts a short-lived Desktop/platform app token from either:

- `?token=<token>` in the URL
- the token field in the page

If a URL token is present, the app reads it once and immediately removes it from browser history. Tokens are not saved to `localStorage`.

## Production Routing

For `*.m.zenmind.cc`:

- WebSocket upgrade requests go to the Relay.
- Normal HTTP requests go to the public static site.

The Nginx and Caddy examples in `../deploy` show this split.
