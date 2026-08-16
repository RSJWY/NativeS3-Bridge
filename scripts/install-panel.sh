#!/usr/bin/env bash
set -euo pipefail

readonly default_install_dir="/opt/natives3-panel"
readonly image_repository="ghcr.io/rsjwy/natives3-panel"

install_dir="$default_install_dir"
panel_host=""
tag="latest"
db_driver="sqlite"
db_dsn=""
db_driver_set=false
db_dsn_set=false
force=false
no_start=false

usage() {
  cat <<'USAGE'
Install a standalone NativeS3 panel.

Usage:
  install-panel.sh --panel-host HOST[,HOST...] [options]
  install-panel.sh renew-server-cert --install-dir PATH --panel-host HOST[,HOST...] [options]

Subcommands:
  (default)             Install a new panel
  renew-server-cert     Re-sign the panel server certificate (non-destructive;
                        does not touch panel DB, master.key, or node certs)

Required:
  --panel-host HOST[,HOST...]
                        DNS hostname(s) or IPv4 address(es) nodes use for
                        panel:9443. Multiple values comma-separated (mixed
                        DNS and IPv4 allowed).

Options:
  --install-dir PATH    Installation directory (default: /opt/natives3-panel)
  --tag TAG             GHCR image tag (default: latest)
  --db-driver DRIVER    Database driver: sqlite, mysql (also MariaDB), or postgres (default: sqlite)
  --db-dsn DSN          Database DSN. Default: /data/panel.db (sqlite). For
                        mysql/postgres pass the full connection string, or in
                        an interactive terminal leave it unset to be prompted
                        for host/port/user/password/dbname. Written into
                        panel.yaml and never echoed.
  --force               Replace an existing installation directory
  --no-start            Generate and validate files without pulling or starting
  -h, --help            Show this help

renew-server-cert options:
  --install-dir PATH    (required) Existing installation directory
  --panel-host HOST[,HOST...]
                        (required) New SAN list for the server certificate
  --days N              Certificate validity in days (default: 825)
  --restart             Restart the panel container after successful re-sign
                        (default: do not restart; the panel must be restarted
                        manually for the new certificate to take effect)

When attached to a terminal, a missing --panel-host and any unset database
options are prompted for; sqlite defaults to /data/panel.db. In a
non-interactive pipeline --panel-host is required and database options fall
back to sqlite + /data/panel.db unless overridden.
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

# build_san takes a comma-separated list of hostnames/IPv4 addresses and outputs
# the OpenSSL subjectAltName value (e.g. "DNS:foo.example.com,IP:10.0.0.1").
# Each item is independently classified using is_ipv4 / is_dns_name. Any item
# that matches neither causes die (no silent drop, R2.3). An empty overall list
# also dies. This is the single source of truth for SAN construction — install
# and renew-server-cert both call it.
build_san() {
  local input="$1" item san=""
  IFS=',' read -r -a _san_items <<<"$input"
  for item in "${_san_items[@]}"; do
    item="${item## }"
    item="${item%% }"
    [[ -n "$item" ]] || continue
    if is_ipv4 "$item"; then
      san="${san:+$san,}IP:$item"
    elif is_dns_name "$item"; then
      san="${san:+$san,}DNS:$item"
    else
      die "--panel-host entry is not a valid DNS hostname or IPv4 address: $item"
    fi
  done
  [[ -n "$san" ]] || die "--panel-host must contain at least one valid DNS hostname or IPv4 address"
  printf '%s' "$san"
}

validate_db_driver() {
  case "$1" in
    sqlite|mysql|postgres) ;;
    *) die "invalid --db-driver: $1 (expected sqlite, mysql, or postgres)" ;;
  esac
}

validate_db_dsn() {
  local driver="$1" dsn="$2"
  [[ -n "$dsn" ]] || die "--db-dsn may not be empty"
  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* ]] || die "--db-dsn may not contain newlines"
}

yaml_quote() {
  local value="$1"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "values may not contain newlines"
  value=${value//\'/\'\'}
  printf "'%s'" "$value"
}

url_encode() {
  # 百分号编码：把 mysql/postgres DSN 里的 user/password 编码成安全字符。
  # 仅按字节处理，假设凭据为 ASCII（DSN 凭据的常规情况）。
  local s="$1" i c out=""
  for ((i = 0; i < ${#s}; i++)); do
    c="${s:$i:1}"
    case "$c" in
      [A-Za-z0-9.~-]) out+="$c" ;;
      *) printf -v c '%%%02X' "'$c"; out+="$c" ;;
    esac
  done
  printf '%s' "$out"
}

# prompt_external_dsn 交互式逐项收集外部数据库连接信息并拼出 DSN，直接写入全局 db_dsn。
# 仅在交互终端、db_driver 非 sqlite 且未通过 --db-dsn 显式提供时调用。不做格式校验，
# 用户输入原样用于拼接（凭据经 url_encode 以保证 DSN 结构不被特殊字符破坏）。
prompt_external_dsn() {
  local driver="$1" default_dbname="$2"
  local host port user pass dbname sslmode
  case "$driver" in
    mysql)
      read -r -p "Database host (default 127.0.0.1): " host
      host="${host:-127.0.0.1}"
      read -r -p "Database port (default 3306): " port
      port="${port:-3306}"
      read -r -p "Database name (default $default_dbname): " dbname
      dbname="${dbname:-$default_dbname}"
      read -r -p "Database user: " user
      read -r -p "Database password: " pass
      db_dsn="$(url_encode "$user"):$(url_encode "$pass")@tcp($host:$port)/$dbname?parseTime=true&charset=utf8mb4"
      ;;
    postgres)
      read -r -p "Database host (default 127.0.0.1): " host
      host="${host:-127.0.0.1}"
      read -r -p "Database port (default 5432): " port
      port="${port:-5432}"
      read -r -p "Database name (default $default_dbname): " dbname
      dbname="${dbname:-$default_dbname}"
      read -r -p "Database user: " user
      read -r -p "Database password: " pass
      read -r -p "SSL mode (default disable): " sslmode
      sslmode="${sslmode:-disable}"
      db_dsn="postgres://$(url_encode "$user"):$(url_encode "$pass")@$host:$port/$dbname?sslmode=$sslmode"
      ;;
  esac
}

# --- renew-server-cert subcommand ---
# Re-signs the panel server certificate in-place. Non-destructive: only writes
# under <install_dir>/data/pki/. Never touches panel DB, master.key,
# intermediate-ca.key, or node_certs data (red line, structural protection).
renew_server_cert() {
  local rc_install_dir="" rc_panel_host="" rc_days=825 rc_restart=false
  while (($# > 0)); do
    case "$1" in
      --install-dir)
        (($# >= 2)) || die "--install-dir requires a value"
        rc_install_dir="$2"
        shift 2
        ;;
      --panel-host)
        (($# >= 2)) || die "--panel-host requires a value"
        rc_panel_host="$2"
        shift 2
        ;;
      --days)
        (($# >= 2)) || die "--days requires a value"
        rc_days="$2"
        shift 2
        ;;
      --restart)
        rc_restart=true
        shift
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "renew-server-cert: unknown argument: $1"
        ;;
    esac
  done

  [[ -n "$rc_install_dir" ]] || die "renew-server-cert: --install-dir is required"
  [[ -n "$rc_panel_host" ]] || die "renew-server-cert: --panel-host is required"
  validate_install_dir "$rc_install_dir"

  local pki_dir="$rc_install_dir/data/pki"
  local ca_crt="$pki_dir/intermediate-ca.crt"
  local ca_key="$pki_dir/intermediate-ca.key"
  local server_crt="$pki_dir/panel-server.crt"
  local server_key="$pki_dir/panel-server.key"

  # --- Pre-flight checks (all must pass before any write) ---
  [[ "$(id -u)" -eq 0 ]] || die "run this command as root (for example with sudo)"
  require_command openssl
  [[ -f "$ca_crt" && -r "$ca_crt" ]] || die "renew-server-cert: CA cert not found or not readable: $ca_crt"
  [[ -f "$ca_key" && -r "$ca_key" ]] || die "renew-server-cert: CA key not found or not readable: $ca_key"
  [[ -f "$server_crt" ]] || die "renew-server-cert: existing server cert not found: $server_crt (run install first)"

  local san
  san="$(build_san "$rc_panel_host")"

  umask 077

  local ts
  ts="$(date +%Y%m%d%H%M%S)"
  local bak_crt="${server_crt}.bak.${ts}"
  local bak_key="${server_key}.bak.${ts}"

  # --- Backup existing cert+key (R4.1) ---
  cp -- "$server_crt" "$bak_crt"
  cp -- "$server_key" "$bak_key"
  chmod 600 "$bak_key"

  # --- Generate new cert using temp filenames (R4.2: fail → originals untouched) ---
  local tmp_key="$pki_dir/.panel-server-renew.key"
  local tmp_csr="$pki_dir/.panel-server-renew.csr"
  local tmp_ext="$pki_dir/.panel-server-renew.ext"
  local tmp_crt="$pki_dir/.panel-server-renew.crt"

  # Clean up temp files on exit (success or failure)
  trap 'rm -f -- "$tmp_key" "$tmp_csr" "$tmp_ext" "$tmp_crt" "$pki_dir/intermediate-ca.srl"' EXIT

  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 \
    -out "$tmp_key" >/dev/null 2>&1 || die "renew-server-cert: failed to generate new private key"
  openssl req -new -sha256 \
    -key "$tmp_key" \
    -subj "/CN=$(printf '%s' "$rc_panel_host" | cut -d, -f1)" \
    -out "$tmp_csr" 2>/dev/null || die "renew-server-cert: failed to generate CSR"
  cat >"$tmp_ext" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$san
EOF
  openssl x509 -req -sha256 -days "$rc_days" \
    -in "$tmp_csr" \
    -CA "$ca_crt" \
    -CAkey "$ca_key" \
    -CAcreateserial \
    -extfile "$tmp_ext" \
    -out "$tmp_crt" >/dev/null 2>&1 || die "renew-server-cert: failed to sign new certificate"

  # --- Validate new cert before replacing (R4.5 / AC9) ---
  openssl x509 -in "$tmp_crt" -noout >/dev/null 2>&1 || die "renew-server-cert: new certificate is not parseable"
  local san_check
  san_check="$(openssl x509 -in "$tmp_crt" -noout -text | grep -A1 'Subject Alternative Name' | tail -1 | sed 's/^[[:space:]]*//')" || true
  [[ -n "$san_check" ]] || die "renew-server-cert: new certificate has no SAN extension"
  openssl verify -CAfile "$ca_crt" "$tmp_crt" >/dev/null 2>&1 || die "renew-server-cert: new certificate does not verify against CA"

  # --- Atomic replace (R4.6) ---
  mv -f -- "$tmp_crt" "$server_crt"
  mv -f -- "$tmp_key" "$server_key"
  chmod 600 "$server_key"
  chmod 644 "$server_crt"
  chown 10001:10001 "$server_crt" "$server_key"

  # --- Clean up intermediate files (R4.4 / AC11) ---
  rm -f -- "$tmp_csr" "$tmp_ext" "$pki_dir/intermediate-ca.srl"
  trap - EXIT

  # --- Output (R4.5 / R5.2 / R5.3) ---
  local new_expiry
  new_expiry="$(openssl x509 -in "$server_crt" -noout -enddate | cut -d= -f2)"
  cat <<EOF
Panel server certificate re-signed successfully.

  SAN:          $san_check
  Expires:      $new_expiry
  Install dir:  $rc_install_dir

Backups (delete after confirming the new certificate works):
  $bak_crt
  $bak_key

Rollback (if the new certificate has problems):
  mv -f "$bak_crt" "$server_crt"
  mv -f "$bak_key" "$server_key"
  chown 10001:10001 "$server_crt" "$server_key"
  chmod 600 "$server_key"
  chmod 644 "$server_crt"
  Then restart the panel container.

IMPORTANT: The panel loads the server certificate at process startup.
The new certificate will NOT take effect until the panel container is restarted.

To restart now:
  docker compose --project-directory "$rc_install_dir" -f "$rc_install_dir/docker-compose.yml" restart panel

Re-signing the server certificate does NOT invalidate already-registered nodes.
The CA and node client certificates are unchanged; nodes will reconnect without re-registration.
EOF

  if [[ "$rc_restart" == true ]]; then
    echo "Restarting panel container (--restart)..."
    docker compose --project-directory "$rc_install_dir" -f "$rc_install_dir/docker-compose.yml" restart panel
  else
    echo "Use --restart to automatically restart the panel container after re-signing."
  fi
}

# --- Subcommand dispatch ---
# If $1 is a known subcommand, shift and dispatch; otherwise fall through to
# the default install path. This keeps the install body un-reindented (design
# §1.1).
if [[ $# -gt 0 && "$1" == "renew-server-cert" ]]; then
  shift
  renew_server_cert "$@"
  exit 0
fi

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
    --db-driver)
      (($# >= 2)) || die "--db-driver requires a value"
      db_driver="$2"
      db_driver_set=true
      shift 2
      ;;
    --db-dsn)
      (($# >= 2)) || die "--db-dsn requires a value"
      db_dsn="$2"
      db_dsn_set=true
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
    read -r -p "Panel hostname or IPv4 address used by nodes (comma-separated for multiple): " panel_host
  else
    die "--panel-host is required in non-interactive mode"
  fi
fi

if [[ -t 0 && "$db_driver_set" != true ]]; then
  read -r -p "Database driver [sqlite/mysql/postgres] (default sqlite): " db_driver
  db_driver="${db_driver:-sqlite}"
fi
if [[ -t 0 && "$db_dsn_set" != true ]]; then
  if [[ "$db_driver" == "sqlite" ]]; then
    read -r -p "Database DSN (default /data/panel.db): " db_dsn
    db_dsn="${db_dsn:-/data/panel.db}"
  else
    prompt_external_dsn "$db_driver" "panel"
  fi
fi

# 非交互或交互未输入时的兜底：sqlite 给默认路径，非 sqlite 必须显式提供
if [[ "$db_driver" != "sqlite" && -z "$db_dsn" ]]; then
  die "--db-dsn is required for $db_driver"
fi
if [[ "$db_driver" == "sqlite" && -z "$db_dsn" ]]; then
  db_dsn="/data/panel.db"
fi

validate_install_dir "$install_dir"
validate_tag "$tag"
validate_db_driver "$db_driver"
validate_db_dsn "$db_driver" "$db_dsn"
quoted_db_dsn="$(yaml_quote "$db_dsn")"
san="$(build_san "$panel_host")"

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
  driver: "$db_driver"
  dsn: $quoted_db_dsn

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
Database driver:             $db_driver

Bootstrap admin password (save it now): $admin_password
Admin UI (local host only): http://127.0.0.1:9001/
Node control endpoint:       https://$panel_host:9443
Public CA to copy to nodes:  $install_dir/panel-ca.crt

After first login, follow the deployment guide to replace the bootstrap password
with its logged bcrypt hash and clear admin_bootstrap_password.
EOF
if [[ "$db_driver" != "sqlite" ]]; then
  cat <<EOF

Note: panel is configured for $db_driver. Ensure the container can reach the
database host (use a reachable host/IP, not localhost, unless using host
networking). The DSN is in $install_dir/panel.yaml.
EOF
fi
if [[ "$no_start" == true ]]; then
  printf '\nFiles were generated but the image was not pulled and the service was not started.\n'
fi
