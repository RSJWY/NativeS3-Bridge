# Design

## Boundary

This task validates existing UI behavior. It should not redesign components or alter user
flows unless validation reveals a concrete defect.

## Validation Options

Preferred order:

1. Use a small repeatable browser automation script if the repo already has browser test
   dependencies or if Chrome DevTools can perform the validation without adding packages.
2. Use Chrome DevTools manual automation in this session and document the exact flows.
3. Add Playwright only if dependency installation is already available and the added
   maintenance cost is justified.

## Local Service Setup

Use a temporary config, loopback ports, SQLite DB, and temporary data root. Configure a
known admin bootstrap password, start the binary, capture the generated bcrypt hash, then
write a config with `password_hash` or restart if necessary.

## Secret Handling

Avoid logging cookies, passwords, generated secret keys, or full presigned URLs. UI
assertions can check presence of modal fields without copying their values into reports.

## Rollback

Rollback is limited to optional validation scripts/docs unless a real UI bug is found.
