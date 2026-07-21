# Design: Reliable Registration And Health

## Registration Idempotency

Registration identity is the tuple `(token hash, node ID, public-key fingerprint)`. The Panel parses and validates the CSR, hashes its PKIX public key, and performs the following inside one DB transaction with a row lock where supported:

1. Load token by hash and verify node binding/expiry.
2. If unused, verify node lifecycle, sign the CSR, persist one `NodeCert`, and update the token with `used_at`, public-key fingerprint, issued certificate PEM, signing CA PEM, and expiry.
3. If already used with the same public-key fingerprint and a stored issued response, return that response without signing or inserting again.
4. If already used with a different public key, reject with the existing coarse `401 registration denied` response.

Both registration and mTLS certificate validation require node lifecycle `active`. `disabled` is a reversible control-plane pause: it rejects reconnects without revoking certificates, so switching back to `active` restores connectivity. `retired` remains irreversible.

Certificate PEM and CA PEM are public identity material, not secrets. Storing them beside the token makes response replay possible without storing node private keys or token plaintext.

## Node Retry State Machine

`Register` returns a typed HTTP rejection for non-2xx responses. A registration loop owns retry policy:

- retry: transport/TLS errors, HTTP 429, HTTP 5xx;
- terminal for the current process/config: HTTP 400, 401, 403 and other non-retryable 4xx;
- cancellation immediately stops backoff.

The loop reuses the same on-disk private key. CSR signatures may differ across attempts, but public-key fingerprint remains stable and is the Panel idempotency key.

## Health Model

- Panel `/healthz`: process handler is alive, no DB dependency.
- Panel `/readyz`: Panel DB ping succeeds; returns 503 otherwise.
- Node Docker health: a new CLI health mode loads config, normalizes wildcard bind addresses to loopback, sends an HTTP request to the S3 listener, and accepts a valid HTTP response including S3 `403 AccessDenied`.
- Control-plane online/offline remains observed in Panel. Panel downtime must not fail Node data-plane health.

## Compatibility And Migration

New nullable registration-token columns are additive and auto-migrated. Existing unused tokens work normally. Existing used tokens without replay material remain non-replayable and require issuing a fresh token, which matches their current behavior.

## Rollback

Rollback can ignore the additive token columns. Node retry logic and health CLI are isolated; existing `-check-config` remains available.
