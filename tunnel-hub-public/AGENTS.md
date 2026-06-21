# AGENTS.md

This project is the lightweight public web client for ZenMind Desktop public hosts under `*.m.zenmind.cc`.

## Purpose

- Serve a responsive browser UI when a Desktop public host is opened as normal HTTP.
- Keep Desktop WebSocket traffic on `wss://<host>/ws` compatible with the existing Tunnel Hub Relay and Desktop WS protocol.
- Provide a small, practical client for Desktop task board and agent-platform access; this is not the full Desktop app and not the admin console.

## Stack

- React + TypeScript + Vite.
- Vitest + jsdom + Testing Library.
- `lucide-react` for icons.
- Nginx serves the built static SPA in production.

## Important Boundaries

- Do not store Desktop/platform auth tokens in `localStorage`, source, docs, snapshots, or logs.
- URL `?token=` may be consumed at startup, but it must be removed from browser history immediately.
- Default WebSocket target is same-host `/ws`; do not hard-code production random hosts.
- Do not change the Tunnel Hub server API or Desktop WS protocol from this project.
- Keep the UI compact, responsive, and operational. Avoid marketing-style landing pages.

## Development

```bash
npm install
npm test
npm run build
npm run dev
```

The dev server listens on `127.0.0.1:11965`.
