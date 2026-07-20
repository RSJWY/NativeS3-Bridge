#!/usr/bin/env bash
set -euo pipefail

readonly default_install_dir="/opt/natives3-panel"
readonly image_repository="ghcr.io/rsjwy/natives3-panel"

install_dir="$default_install_dir"
panel_host=""
tag="latest"
force=false
no_start=false

usage() {
  cat <<'USAGE'
Install a standalone NativeS3 panel.

Usage:
  install-panel.sh --panel-host HOST [options]

Required:
  --panel-host HOST       DNS hostname or IPv4 address nodes use for panel:9443

Options:
  --install-dir PATH      Installation directory (default: /opt/natives3-panel)
  --tag TAG               GHCR image tag (default: latest)
  --force                 Replace an existing installation directory
  --no-start              Generate and validate files without pulling or starting
  -h, --help              Show this help

When attached to a terminal, a missing --panel-host is prompted for. In a
non-interactive pipeline it is an error.
USAGE
}

die() {
  printf 'install-panel: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

validate_tag() {
  [[ "$1" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid image tag: $1"
}

is_ipv4() {
  local value="$1" part
  local -a parts
  IFS='.' read -r -a parts <<<"$value"
  [[ ${#parts[@]} -eq 4 ]] || return 1
  for part in "${parts[@]}"; do
    [[ "$part" =~ ^[0-9]{1,3}$ ]] || return 1
    ((10#$part <= 255)) || return 1
  done
}

is_dns_name() {
  local value="$1" label
  [[ ${#value} -le 253 && "$value" =~ ^[A-Za-z0-9.-]+$ ]] || return 1
  [[ "$value" != .* && "$value" != *. && "$value" != *..* ]] || return 1
  IFS='.' read -r -a labels <<<"$value"
  for label in "${labels[@]}"; do
    [[ ${#label} -le 63 && "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ ]] || return 1
  done
}

validate_install_dir() {
  [[ "$1" == /* ]] || die "--install-dir must be an absolute path"
  [[ "/${1#/}/" != *"/./"* && "/${1#/}/" != *"/../"* ]] || \
    die "--install-dir may not contain . or .. path components"
  case "${1%/}" in
    ""|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var)
      die "refusing unsafe installation directory: $1"
      ;;
  esac
}

while (($# > 0)); do
  case "$1" in
    --panel-host)
      (($# >= 2)) || die "--panel-host requires a value"
      panel_host="$2"
      shift 2
      ;;
    --install-dir)
      (($# >= 2)) || die "--install-dir requires a value"
      install_dir="$2"
      shift 2
      ;;
    --tag)
      (($# >= 2)) || die "--tag requires a value"
      tag="$2"
      shift 2
      ;;
    --force)
      force=true
      shift
      ;;
    --no-start)
      no_start=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

if [[ -z "$panel_host" ]]; then
  if [[ -t 0 ]]; then
    read -r -p "Panel hostname or IPv4 address used by nodes: " panel_host
  else
    die "--panel-host is required in non-interactive mode"
  fi
fi

validate_install_dir "$install_dir"
validate_tag "$tag"
if is_ipv4 "$panel_host"; then
  san="IP:$panel_host"
elif is_dns_name "$panel_host"; then
  san="DNS:$panel_host"
else
  die "--panel-host must be a valid DNS hostname or IPv4 address"
fi

[[ "$(id -u)" -eq 0 ]] || die "run this installer as root (for example with sudo)"
require_command openssl
require_command docker
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"

if [[ -e "$install_dir" || -L "$install_dir" ]]; then
  if [[ "$force" != true ]]; then
    die "$install_dir already exists; use --force to replace it"
  fi
  rm -rf -- "$install_dir"
fi

umask 077
mkdir -p "$install_dir/data/pki" "$install_dir/data/secrets"

openssl rand -out "$install_dir/data/secrets/master.key" 32
admin_password="$(openssl rand -hex 16)"
session_secret="$(openssl rand -hex 32)"

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 \
  -out "$install_dir/data/pki/intermediate-ca.key" >/dev/null 2>&1
openssl req -x509 -new -sha256 -days 3650 \
  -key "$install_dir/data/pki/intermediate-ca.key" \
  -subj "/CN=NativeS3 Deployment CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out "$install_dir/data/pki/intermediate-ca.crt"

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 \
  -out "$install_dir/data/pki/panel-server.key" >/dev/null 2>&1
openssl req -new -sha256 \
  -key "$install_dir/data/pki/panel-server.key" \
  -subj "/CN=$panel_host" \
  -out "$install_dir/data/pki/panel-server.csr"
cat >"$install_dir/data/pki/panel-server.ext" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$san
EOF
openssl x509 -req -sha256 -days 825 \
  -in "$install_dir/data/pki/panel-server.csr" \
  -CA "$install_dir/data/pki/intermediate-ca.crt" \
  -CAkey "$install_dir/data/pki/intermediate-ca.key" \
  -CAcreateserial \
  -extfile "$install_dir/data/pki/panel-server.ext" \
  -out "$install_dir/data/pki/panel-server.crt" >/dev/null 2>&1
rm -f -- "$install_dir/data/pki/panel-server.csr" \
  "$install_dir/data/pki/panel-server.ext" \
  "$install_dir/data/pki/intermediate-ca.srl"
cp -- "$install_dir/data/pki/intermediate-ca.crt" "$install_dir/panel-ca.crt"

cat >"$install_dir/panel.yaml" <<EOF
admin_addr: "0.0.0.0:9001"

agent:
  addr: "0.0.0.0:9443"
  cert_file: "/data/pki/panel-server.crt"
  key_file: "/data/pki/panel-server.key"

pki:
  intermediate_cert_file: "/data/pki/intermediate-ca.crt"
  intermediate_key_file: "/data/pki/intermediate-ca.key"
  client_cert_ttl: 2160h

master_key_file: "/data/secrets/master.key"

database:
  driver: "sqlite"
  dsn: "/data/panel.db"

region: "us-east-1"
log_level: "info"
heartbeat_interval: 15s
offline_multiplier: 3

webadmin:
  password_hash: ""
  admin_bootstrap_password: "$admin_password"
  session_secret: "$session_secret"
  session_ttl_minutes: 720
  login_max_failures: 5
  login_lockout_window: 15m
  totp:
    enabled: false
    issuer: "NativeS3-Bridge"
    account: "admin"
    secret: ""
  captcha:
    enabled: false
EOF

cat >"$install_dir/docker-compose.yml" <<EOF
services:
  panel:
    image: $image_repository:$tag
    restart: unless-stopped
    ports:
      - "127.0.0.1:9001:9001"
      - "9443:9443"
    volumes:
      - ./panel.yaml:/app/configs/panel.yaml:ro
      - ./data:/data
    healthcheck:
      test: ["CMD", "/usr/local/bin/panel", "-check-config", "-config", "/app/configs/panel.yaml"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
EOF

chown -R 10001:10001 "$install_dir/data" "$install_dir/panel.yaml"
chmod 700 "$install_dir/data" "$install_dir/data/pki" "$install_dir/data/secrets"
chmod 600 "$install_dir/panel.yaml" \
  "$install_dir/data/secrets/master.key" \
  "$install_dir/data/pki/intermediate-ca.key" \
  "$install_dir/data/pki/panel-server.key"
chmod 644 "$install_dir/docker-compose.yml" \
  "$install_dir/panel-ca.crt" \
  "$install_dir/data/pki/intermediate-ca.crt" \
  "$install_dir/data/pki/panel-server.crt"

compose=(docker compose --project-directory "$install_dir" -f "$install_dir/docker-compose.yml")
"${compose[@]}" config --quiet

if [[ "$no_start" != true ]]; then
  "${compose[@]}" pull panel
  "${compose[@]}" up -d panel
fi

cat <<EOF
NativeS3 panel files were created in $install_dir.

Bootstrap admin password (save it now): $admin_password
Admin UI (local host only): http://127.0.0.1:9001/
Node control endpoint:       https://$panel_host:9443
Public CA to copy to nodes:  $install_dir/panel-ca.crt

After first login, follow the deployment guide to replace the bootstrap password
with its logged bcrypt hash and clear admin_bootstrap_password.
EOF
if [[ "$no_start" == true ]]; then
  printf '\nFiles were generated but the image was not pulled and the service was not started.\n'
fi
