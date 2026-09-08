# Device-link V1 routing boundary

Identity Center is the account-device authority. Tunnel Hub maintains only a projection and live Desktop presence.

## Presence merge

1. Preserve Identity list order and all non-deleted records.
2. Never add a device that is absent from the current Identity projection.
3. Mobile records never expose connectable `online` state.
4. For Desktop records, a live authenticated tunnel sets `online=true` and `connectedAt`; no live tunnel sets `online=false` while retaining `lastConnectedAt`.
5. `activeDesktopId` must resolve to an online Desktop in the same authoritative account list. A known offline Desktop returns `TARGET_OFFLINE`; a mobile, unknown ID or unsafe enum returns `PROTOCOL_INCOMPATIBLE` or the more specific authenticated account/revocation error.

The merge is a single O(n) pass with O(1) presence lookup and does not resort paginated Identity results.

## Route credential

After validating that source and target are active members of the same account, Hub signs a two-minute RS256 credential. The protected header contains `kid`; claims bind `iss`, `aud`, `accountId`, `sourceDeviceId`, `targetDesktopId`, `requestId`, `jti`, `iat` and `exp`.

Desktop verifies the signature, expected issuer/audience, its own target ID, current account state, expiry and unused JTI. A JTI is retained until credential expiry. Rotation publishes the new public key before signing with it and keeps the previous public key available for one full credential TTL. Missing state, target mismatch, expiry, signature failure or replay returns `ROUTE_CREDENTIAL_INVALID` and closes the route.

Hub does not trust the account claims in an otherwise valid cached JWT as current state. Before registration, presence reads, route issuance, and account-device validation it calls Identity Center's authenticated validation endpoint. Existing Desktop tunnel sessions are revalidated every three seconds and closed on rejection or validation failure. Set `IDENTITY_API_BASE_URL` to an HTTPS Identity origin (loopback HTTP is accepted only for local development); if it is absent or unavailable, account-device operations fail closed.

The client-facing route response contains the target's `wss://.../ws` URL and `tokenMode`. A route credential is one-time: physical reconnect and in-band `auth.refresh` both require a newly issued credential. Hub exposes only the current and one overlap public key at `/api/desktop/route-credential-jwks`; neither credentials nor bearer tokens are written to logs.

Hub does not persist conversation, message, task, attachment, workspace, artifact or execution-result payloads.
