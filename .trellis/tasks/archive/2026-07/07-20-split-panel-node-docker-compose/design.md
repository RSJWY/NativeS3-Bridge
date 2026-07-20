# Technical Design

## Boundaries

This change only affects deployment assets and documentation. Runtime panel/node behavior, configuration schemas, PKI code, and release publishing remain unchanged.

## Deployment Assets

- `docker-compose.panel.yml`: standalone panel template using `ghcr.io/rsjwy/natives3-panel:${NATIVES3_TAG:-latest}`.
- `docker-compose.node.yml`: standalone node template using `ghcr.io/rsjwy/natives3-node:${NATIVES3_TAG:-latest}`.
- `scripts/install-panel.sh`: no-clone installer that creates `/opt/natives3-panel` by default, generates panel config, master key, a self-signed deployment CA, and a panel listener certificate whose SAN matches the supplied hostname/IPv4 address. It writes a local `docker-compose.yml` and starts panel.
- `scripts/install-node.sh`: no-clone installer that creates `/opt/natives3-node` by default, copies the trusted public panel CA, writes node config and `docker-compose.yml`, and starts node.
- `docs/docker-deployment.md`: concise one-click commands plus the complete manual equivalent, lifecycle commands, security notes, and links to the operations guide.

The old combined `docker-compose.example.yml` is removed because it models panel and node on one host and includes local builds and optional databases that conflict with the new quick-deploy goal.

## Generated Files

Panel installer:

```text
/opt/natives3-panel/
├── docker-compose.yml
├── panel.yaml
└── data/
    ├── panel.db
    ├── pki/
    │   ├── intermediate-ca.crt
    │   ├── intermediate-ca.key
    │   ├── panel-server.crt
    │   └── panel-server.key
    └── secrets/master.key
```

Node installer:

```text
/opt/natives3-node/
├── docker-compose.yml
├── node.yaml
└── data/
    ├── natives3.db
    ├── objects/
    └── pki/
        ├── panel-ca.crt
        ├── node.key
        └── node.crt
```

## Security Defaults

- Panel admin port maps to `127.0.0.1:9001` by default.
- Agent control port maps to all interfaces on 9443.
- Node only publishes port 9000.
- The panel CA private key never leaves the panel directory. Only the public `intermediate-ca.crt` is copied to nodes.
- Installer-generated admin password and session secret use OpenSSL randomness.
- Generated private files are mode 600; public certificates are mode 644; data ownership matches image UID/GID 10001.
- Installers refuse to overwrite an existing deployment unless `--force` is explicitly passed.
- `latest` is the convenience default; `--tag` permits version pinning.

## Compatibility and Rollback

- Existing source builds and runtime config loaders are unchanged.
- Users of the former combined Compose file must switch to one template per host.
- Rollback of this documentation/deployment change is restoring the old combined Compose and README section; generated runtime data remains compatible with panel/node images.
