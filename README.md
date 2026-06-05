# NativeS3-Bridge

NativeS3-Bridge exposes a local native directory tree through an S3-compatible API and includes a single-password web administration UI.

## 管理界面安全提示

The admin UI listens on `server.admin_addr`. If `server.tls.enabled` is `false`, the admin UI and session cookie are served over plain HTTP. This is suitable only for trusted local networks or development. For production, enable TLS in the process or place the admin port behind a trusted reverse proxy that terminates HTTPS.

The server logs a warning at startup when TLS is disabled:

```text
admin UI served over plain HTTP; enable TLS for production
```

## 管理端配置

```yaml
webadmin:
  password_hash: "$2a$..."             # bcrypt hash
  admin_bootstrap_password: ""          # first startup only; clear after generating password_hash
  session_secret: "change-me-32bytes-random"
  session_ttl_minutes: 720
```

When `password_hash` is empty and `admin_bootstrap_password` is set, startup generates a bcrypt hash and prints it in the logs. Copy that hash into `password_hash` and clear `admin_bootstrap_password`.

## 构建

Build the embedded Vue admin UI first, then build the Go binary:

```bash
cd pkg/webadmin/ui && npm ci && npm run build
cd ../../..
go build -o natives3bridge ./cmd/natives3bridge
```
