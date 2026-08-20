# Tunnel Hub Public

## Overview

`tunnel-hub-public` is the lightweight browser client for Desktop public hosts such as `https://device.m.example.test/`.

Normal HTTP requests render this responsive mini site. WebSocket upgrades still use `wss://<host>/ws` and are routed by the host reverse proxy to `tunnel-hub-server`.

## Development

```bash
cd tunnel-hub-public
npm install
npm test
BRAND_CONFIG_FILE=../configs/brand.example.yaml npm run build
BRAND_CONFIG_FILE=../configs/brand.example.yaml npm run dev
```

Open `http://127.0.0.1:11965`.

Tests default to `../configs/brand.example.yaml`. Development and production builds default to `../configs/brand.yaml` and intentionally fail while that committed file still contains empty required values. Vite strictly validates the same schema as Relay and safely injects `brand.publicSiteTitle` into the generated page title.

## Auth

The page accepts a short-lived Desktop/platform app token from either:

- `?token=<token>` in the URL
- the token field in the page

If a URL token is present, the app reads it once and immediately removes it from browser history. Tokens are not saved to `localStorage`.

## Production Routing

For `*.m.example.test` (replace with `domains.desktopPublicBase` for the selected brand):

- WebSocket upgrade requests go to the Relay.
- Normal HTTP requests go to the public static site.

The Nginx and Caddy examples in `../deploy` show this split.
