# Webadmin UI implementation decisions

## Session implementation

- Chosen approach: signed HTTP-only cookie.
- Format: base64url JSON payload containing `exp` plus HMAC-SHA256 signature using `webadmin.session_secret`.
- Reason: satisfies single-password session requirements without adding JWT dependencies, keeps backend on `net/http`, and allows browser `fetch(..., credentials: 'include')`.

## State management

- Chosen approach: lightweight Vue composable/reactive module in `src/state/auth.ts`.
- Reason: this UI only needs login state and server-fetched page data, so Pinia would add unnecessary structure for the frozen MVP scope.

## Secret generation format

- Access key: 20 random characters from uppercase letters and digits.
- Secret key: 30 cryptographically random bytes encoded with base64 raw standard encoding, producing 40 characters.
- Storage: saved to the existing `credentials.secret_key` field and returned only from the create API response.

## Bootstrap hash behavior

- If `webadmin.password_hash` is empty and `webadmin.admin_bootstrap_password` is set, startup generates a bcrypt hash in memory and logs it.
- The server does not write back to the YAML file automatically.
- Reason: automatic config mutation risks corrupting comments/formatting and leaking edits in deployment paths. Operators should copy the logged hash into `password_hash` and clear `admin_bootstrap_password`.
