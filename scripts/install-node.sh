#!/usr/bin/env bash
set -euo pipefail

readonly default_install_dir="/opt/natives3-node"
readonly image_repository="ghcr.io/rsjwy/natives3-node"

install_dir="$default_install_dir"
panel_url=""
node_id=""
registration_token=""
ca_file=""
tag="latest"
force=false
no_start=false

usage() {
  cat <<'USAGE'
Install a standalone NativeS3 node.

Usage:
  install-node.sh --panel-url URL --node-id ID --registration-token TOKEN \
    --ca-file PATH [options]

Required:
  --panel-url URL              Reachable panel base URL, e.g. https://panel.example.com:9443
  --node-id ID                 Logical node ID created in the panel
  --registration-token TOKEN   Single-use registration token issued by the panel
  --ca-file PATH               Public panel CA certificate copied from the panel host

Options:
  --install-dir PATH           Installation directory (default: /opt/natives3-node)
  --tag TAG                    GHCR image tag (default: latest)
  --force                      Replace an existing installation directory
  --no-start                   Generate and validate files without pulling or starting
  -h, --help                   Show this help

Missing required values are prompted for only when attached to a terminal.
USAGE
}

die() {
  printf 'install-node: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

validate_tag() {
  [[ "$1" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid image tag: $1"
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
  local -a labels
  [[ ${#value} -le 253 && "$value" =~ ^[A-Za-z0-9.-]+$ ]] || return 1
  [[ "$value" != .* && "$value" != *. && "$value" != *..* ]] || return 1
  IFS='.' read -r -a labels <<<"$value"
  for label in "${labels[@]}"; do
    [[ ${#label} -le 63 && "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ ]] || return 1
  done
}

yaml_quote() {
  local value="$1"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "values may not contain newlines"
  value=${value//\'/\'\'}
  printf "'%s'" "$value"
}

while (($# > 0)); do
  case "$1" in
    --panel-url)
      (($# >= 2)) || die "--panel-url requires a value"
      panel_url="$2"
      shift 2
      ;;
    --node-id)
      (($# >= 2)) || die "--node-id requires a value"
      node_id="$2"
      shift 2
      ;;
    --registration-token)
      (($# >= 2)) || die "--registration-token requires a value"
      registration_token="$2"
      shift 2
      ;;
    --ca-file)
      (($# >= 2)) || die "--ca-file requires a value"
      ca_file="$2"
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

if [[ -t 0 ]]; then
  [[ -n "$panel_url" ]] || read -r -p "Reachable panel base URL (https://host:9443): " panel_url
  [[ -n "$node_id" ]] || read -r -p "Logical node ID: " node_id
  [[ -n "$registration_token" ]] || read -r -s -p "Single-use registration token: " registration_token
  [[ -n "$registration_token" ]] && printf '\n'
  [[ -n "$ca_file" ]] || read -r -p "Path to the public panel CA certificate: " ca_file
fi

[[ -n "$panel_url" ]] || die "--panel-url is required in non-interactive mode"
[[ -n "$node_id" ]] || die "--node-id is required in non-interactive mode"
[[ -n "$registration_token" ]] || die "--registration-token is required in non-interactive mode"
[[ -n "$ca_file" ]] || die "--ca-file is required in non-interactive mode"
[[ "$node_id" =~ ^[1-9][0-9]*$ ]] || die "--node-id must be a positive integer"
[[ "$panel_url" == https://* ]] || die "--panel-url must use https://"
panel_url="${panel_url%/}"
panel_authority="${panel_url#https://}"
[[ -n "$panel_authority" && "$panel_authority" != */* && "$panel_authority" != *\?* && "$panel_authority" != *\#* ]] || \
  die "--panel-url must be a base URL without a path, query, or fragment"
[[ "$panel_authority" != *[$' \t\r\n']* ]] || die "--panel-url may not contain whitespace"
[[ "$panel_authority" != *@* && "$panel_authority" != *:*:* ]] || \
  die "--panel-url must use a DNS hostname or IPv4 address with an optional port"
panel_host="$panel_authority"
if [[ "$panel_authority" == *:* ]]; then
  panel_host="${panel_authority%:*}"
  panel_port="${panel_authority##*:}"
  [[ "$panel_port" =~ ^[0-9]{1,5}$ ]] && ((10#$panel_port >= 1 && 10#$panel_port <= 65535)) || \
    die "--panel-url contains an invalid port"
fi
if ! is_ipv4 "$panel_host" && ! is_dns_name "$panel_host"; then
  die "--panel-url must use a valid DNS hostname or IPv4 address"
fi

validate_install_dir "$install_dir"
validate_tag "$tag"
[[ "$(id -u)" -eq 0 ]] || die "run this installer as root (for example with sudo)"
require_command openssl
require_command docker
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required (docker compose)"
[[ -f "$ca_file" && -r "$ca_file" ]] || die "CA file is not readable: $ca_file"
openssl x509 -in "$ca_file" -noout >/dev/null 2>&1 || die "CA file is not a valid PEM certificate: $ca_file"

if [[ -e "$install_dir" || -L "$install_dir" ]]; then
  if [[ "$force" != true ]]; then
    die "$install_dir already exists; use --force to replace it"
  fi
  rm -rf -- "$install_dir"
fi

umask 077
mkdir -p "$install_dir/data/objects" "$install_dir/data/pki"
cp -- "$ca_file" "$install_dir/data/pki/panel-ca.crt"

register_url="$panel_url/register"
agent_url="wss://$panel_authority/agent"
quoted_register_url="$(yaml_quote "$register_url")"
quoted_agent_url="$(yaml_quote "$agent_url")"
quoted_token="$(yaml_quote "$registration_token")"

cat >"$install_dir/node.yaml" <<EOF
server:
  s3_addr: "0.0.0.0:9000"
  tls:
    enabled: false
    cert_file: ""
    key_file: ""

storage:
  data_root: "/data/objects"
  metadata_suffix: ".s3meta"

database:
  driver: "sqlite"
  dsn: "/data/natives3.db"

region: "us-east-1"
log_level: "info"

panel:
  node_id: $node_id
  register_url: $quoted_register_url
  agent_url: $quoted_agent_url
  registration_token: $quoted_token
  cert_file: "/data/pki/node.crt"
  key_file: "/data/pki/node.key"
  ca_file: "/data/pki/panel-ca.crt"
  heartbeat_interval: 15s
EOF

cat >"$install_dir/docker-compose.yml" <<EOF
services:
  node:
    image: $image_repository:$tag
    restart: unless-stopped
    ports:
      - "9000:9000"
    volumes:
      - ./node.yaml:/app/configs/node.yaml:ro
      - ./data:/data
    healthcheck:
      test: ["CMD", "/usr/local/bin/node", "-check-config", "-config", "/app/configs/node.yaml"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
EOF

chown -R 10001:10001 "$install_dir/data" "$install_dir/node.yaml"
chmod 700 "$install_dir/data" "$install_dir/data/objects" "$install_dir/data/pki"
chmod 600 "$install_dir/node.yaml"
chmod 644 "$install_dir/docker-compose.yml" "$install_dir/data/pki/panel-ca.crt"

compose=(docker compose --project-directory "$install_dir" -f "$install_dir/docker-compose.yml")
"${compose[@]}" config --quiet

if [[ "$no_start" != true ]]; then
  "${compose[@]}" pull node
  "${compose[@]}" up -d node
fi

cat <<EOF
NativeS3 node files were created in $install_dir.
Panel registration URL: $register_url
S3 endpoint:           http://$(hostname):9000
EOF
if [[ "$no_start" == true ]]; then
  printf '\nFiles were generated but the image was not pulled and the service was not started.\n'
else
  cat <<EOF

Watch registration with:
  docker compose --project-directory $install_dir -f $install_dir/docker-compose.yml logs -f node
After registration succeeds, remove registration_token from node.yaml or set it to ''.
EOF
fi
