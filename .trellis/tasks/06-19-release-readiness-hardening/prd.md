# Release readiness hardening

## Goal

Close the release-readiness gaps identified in the current feature-completion review:
secret-safe database logging, broader end-to-end S3 smoke coverage, and browser-level
admin UI validation.

This parent task owns coordination and final integration review only. Code changes must
happen inside independently verifiable child tasks.

## User Value

The project can move from "feature-complete locally" toward "safe to publish and deploy"
with evidence that core workflows work end to end and that credential secrets are not
leaked through normal logging.

## Confirmed Facts

- `go test ./...`, `go vet ./...`, `go build ./...`, and `npm run build --prefix pkg/webadmin/ui` passed in the current workspace.
- Basic aws-cli smoke passed against a temporary local service instance.
- The existing smoke script covers only basic bucket/object operations, not multipart, metadata/tagging, presigned URL expiry, webhook delivery, or admin UI browser flows.
- `pkg/db/db.go` logs full GORM SQL strings at info level, and credential creation/seed SQL includes `secret_key` values.
- The admin UI builds successfully, but no browser-level validation artifact currently exercises login, credential CRUD, bucket ACL, and dashboard rendering.

## Child Task Map

| Child | Purpose | Dependency |
|---|---|---|
| `06-19-secret-safe-db-logging` | Remove credential-secret leakage from DB query logs. | None; do first because later e2e runs create credentials. |
| `06-19-expanded-s3-e2e-smoke` | Add runnable smoke coverage for S3 release-critical flows. | Prefer after logging fix so smoke logs are secret-safe. |
| `06-19-admin-ui-browser-validation` | Validate admin UI in a real browser session. | Prefer after logging fix; can run independently of expanded S3 smoke. |

Dependencies are operational guidance, not implied by parent/child structure.

## Acceptance Criteria

- [ ] All three child tasks are implemented, verified, and individually checked.
- [ ] Final `go test ./...`, `go vet ./...`, `go build ./...`, and `npm run build --prefix pkg/webadmin/ui` pass.
- [ ] At least one local end-to-end smoke run exercises basic object flow plus multipart, metadata/tagging, presigned URL, and webhook behavior.
- [ ] At least one browser-level admin UI validation run confirms login, credential CRUD, bucket ACL, and dashboard rendering.
- [ ] Test or smoke logs do not print credential `secret_key`, seeded `-seed-secret-key`, session secret, auth header, cookies, captcha token, or full presigned query signatures.
- [ ] Any environment limitations, such as missing external MySQL/PostgreSQL instances, are reported explicitly.

## Out of Scope

- Adding IAM, bucket policy, object lock, SSE, lifecycle, replication, RBAC, or multi-user admin support.
- Rewriting existing S3 protocol support beyond what is needed to validate current release-readiness gaps.
- Replacing the current Vue/Vite admin UI stack.
