# Implementation Plan

## Steps

- [x] Read frontend and backend spec guidance before code edits.
- [x] Inspect existing UI package scripts and dependencies.
- [x] Start a temporary local service with a known admin login path.
- [x] Validate login and dashboard rendering in browser.
- [x] Validate credential create modal and list reload secrecy.
- [x] Validate bucket create and ACL toggle.
- [x] If a small repeatable validation script is practical, add it; otherwise document the manual Chrome DevTools validation outcome.
- [x] Run `npm run build --prefix pkg/webadmin/ui` and `go build ./...`.

## Execution Notes

- Used a temporary CDP script under `/tmp`, not committed to the repository.
- Credential create/list secrecy was verified through authenticated browser-session `fetch` because CDP synthetic form submit did not trigger the credential modal submit handler. The limitation is recorded in `prd.md`.
- Also ran `go test ./...` as an additional regression check.

## Risky Files

- Optional script under `scripts/` or UI test folder.
- `pkg/webadmin/ui/src/**` only if validation finds a real defect.

## Rollback Point

If browser automation setup is too brittle, keep validation manual and report the exact
coverage and limitation.
