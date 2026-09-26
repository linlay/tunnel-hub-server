# Tunnel Hub Public

## Overview

`tunnel-hub-public` is the lightweight browser client for Desktop public hosts such as `https://device.m.example.test/`.

Normal HTTP requests render this responsive mini site. WebSocket upgrades still use `wss://<host>/ws` and are routed by the host reverse proxy to `tunnel-hub-server`.

## Development

```bash
cd tunnel-hub-public
npm install
npm test
npm run build
PUBLIC_SITE_TITLE="Example Public Site" npm run dev
```

Open `http://127.0.0.1:11965`.

The static build is environment-neutral. During local development, Vite exposes `PUBLIC_SITE_TITLE` from the repository `.env` or process environment at `/runtime/public-site-title.txt`. In production, the Nginx entrypoint writes the same plain-text runtime resource; a missing title makes the container fail before Nginx starts.

## Auth

The page accepts a short-lived Desktop/platform app token from either:

- `?token=<token>` in the URL
- the token field in the page

If a URL token is present, the app reads it once and immediately removes it from browser history. Tokens are not saved to `localStorage`.

## Production Routing

For `*.m.example.test` (replace with `DESKTOP_PUBLIC_BASE_DOMAIN` for the selected environment):

- WebSocket upgrade requests go to the Relay.
- Normal HTTP requests go to the public static site.

The Nginx and Caddy examples in `../deploy` show this split.
