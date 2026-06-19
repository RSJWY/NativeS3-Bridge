# Admin UI browser validation

## Goal

Add or perform browser-level validation for admin login, credential CRUD, bucket ACL, and
dashboard rendering.

## User Value

The embedded admin interface is verified as a real browser workflow, not only as compiled
Vue code and backend API tests.

## Confirmed Facts

- `npm run build --prefix pkg/webadmin/ui` passes.
- Existing Go tests cover admin API behavior.
- The README includes screenshots, but no repeatable browser validation script exists.
- Chrome DevTools tooling is available in this session for manual browser validation.

## Requirements

- Validate these browser workflows against a running local service:
  - login with bootstrap-generated password hash or configured test password;
  - dashboard loads and renders summary/chart regions without blank primary content;
  - credentials page can create a credential and shows the one-time secret modal;
  - credentials list does not show `secret_key` after modal close/list reload;
  - bucket page can create a bucket and change ACL between private and public-read.
- Prefer a repeatable script or documented validation artifact over an ad hoc manual check.
- Do not commit screenshots unless they are intentionally updating documentation.
- Do not print session cookies, passwords, or secret keys in logs.

## Acceptance Criteria

- [x] Browser validation has been run against a locally started service.
- [x] Login, dashboard, credential create/list behavior, and bucket ACL behavior are verified.
- [x] The final report includes the validation method and any limitations.
- [x] `npm run build --prefix pkg/webadmin/ui` and `go build ./...` pass after any changes.

## Validation Notes

- Ran a temporary local service from `/tmp/natives3bridge-ui-smoke` with loopback admin/S3 ports, temporary SQLite database, temporary data root, and bootstrap password configured only for the smoke session.
- Headless Chrome + CDP validated login to `/dashboard`, dashboard text, and three `.chart-box` elements.
- In the authenticated browser session, credential create API returned a one-time `secret_key`, and `GET /api/admin/credentials` returned rows without `secret_key`.
- Browser UI validated bucket creation through `/buckets`, ACL switch to `public-read`, and ACL switch back to `private`.
- Limitation: synthetic CDP click/submit did not trigger the credential modal form submit, even though the form was valid and Vue `onSubmit` invoker existed. The one-time secret modal itself was therefore not proven via a real button click in this run; the create/list secrecy contract was verified through the same browser session cookie and admin API.
- Validation commands passed: `npm run build --prefix pkg/webadmin/ui`, `go build ./...`, and `go test ./...`.

## Out of Scope

- Redesigning the UI.
- Adding a full Playwright test suite unless it is the smallest reliable path.
- Testing every responsive breakpoint.
