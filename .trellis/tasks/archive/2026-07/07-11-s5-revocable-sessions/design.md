# Design: Revocable admin sessions

## Boundary

Keep the signed cookie format but add a random `sid`. `Auth` owns an in-memory map of active session IDs to expiry timestamps. Signature validation alone is insufficient: middleware also requires the ID to exist with the matching expiry.

## Lifecycle

1. Login generates 32 random bytes, encodes them with base64url, stores `sid -> expiry`, then signs `{sid, exp}`.
2. Middleware verifies signature and expiry, checks the active map, and attaches the payload to request context.
3. Logout removes the current `sid`, then expires the browser cookie.
4. A new process starts with an empty registry, invalidating all cookies from the prior process even if `session_secret` is unchanged.

## Cleanup and concurrency

A mutex protects the registry. Session issuance and verification opportunistically remove expired records, avoiding a lifecycle-managed cleanup goroutine for the small single-admin session set.

## Compatibility

Legacy cookies without `sid` fail closed and require login again. No database migration is needed.
