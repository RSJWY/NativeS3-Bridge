#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Real Panel -> Node release gate. The scenario is shared by local-process and
# Docker runtime adapters; generated credentials, cookies, and PKI stay in a
# mode-700 temporary directory and are deleted by the exit trap.

IFS=$'\n\t'
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

MODE="${E2E_MODE:-local}"
TIMEOUT="${E2E_TIMEOUT:-90}"
SKIP_BUILD="${E2E_SKIP_BUILD:-0}"
SKIP_BROWSER="${E2E_SKIP_BROWSER:-0}"
REPORT_PATH="${E2E_REPORT:-}"

usage() {
	printf '%s\n' \
		'Usage: scripts/test-panel-node-e2e.sh [--mode local|docker|auto] [--timeout SECONDS]' \
		'       [--skip-build] [--skip-browser] [--report PATH]'
}

while (($#)); do
	case "$1" in
		--mode) MODE="${2:?--mode requires a value}"; shift 2 ;;
		--timeout) TIMEOUT="${2:?--timeout requires seconds}"; shift 2 ;;
		--skip-build) SKIP_BUILD=1; shift ;;
		--skip-browser) SKIP_BROWSER=1; shift ;;
		--report) REPORT_PATH="${2:?--report requires a path}"; shift 2 ;;
		-h|--help) usage; exit 0 ;;
		*) printf 'unknown option: %s\n' "$1" >&2; usage >&2; exit 2 ;;
	esac
done

case "$MODE" in
	local|docker|auto) ;;
	*) printf 'invalid E2E mode: %s\n' "$MODE" >&2; exit 2 ;;
esac

if [[ ! "$TIMEOUT" =~ ^[1-9][0-9]*$ ]]; then
	printf 'invalid E2E timeout: %s (expected a positive integer)\n' "$TIMEOUT" >&2
	exit 2
fi

if [[ "$MODE" == auto ]]; then
	if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
		MODE=docker
	else
		MODE=local
	fi
fi

TMP_DIR=""
PANEL_PID=""
NODE_PID=""
WRONG_NODE_PID=""
PANEL_CONTAINER=""
NODE_CONTAINER=""
WRONG_NODE_CONTAINER=""
DOCKER_NETWORK=""
REPORT_LINES=()
DIAGNOSTICS_COLLECTED=0
PANEL_BIN=""
NODE_BIN=""
PANEL_IMAGE=""
NODE_IMAGE=""
PANEL_CONFIG=""
NODE_CONFIG=""
WRONG_NODE_CONFIG=""
PANEL_LOG=""
NODE_LOG=""
WRONG_NODE_LOG=""
PANEL_ADMIN_PORT=""
PANEL_AGENT_PORT=""
NODE_S3_PORT=""
WRONG_NODE_S3_PORT=""
PANEL_ADMIN_URL=""
NODE_S3_URL=""
COOKIE_FILE=""
API_BODY=""
API_STATUS=""
API_ERROR=""
S3_STATUS=""
S3_ERROR=""
ADMIN_PASSWORD=""
SESSION_SECRET=""
REGISTRATION_TOKEN=""
ACCESS_KEY=""
SECRET_KEY=""
AWS_REGION="us-east-1"
NODE_NAME=""
BUCKET_NAME=""
E2E_RUN_ID=""
DOCKER_USER=""

redact_text() {
	local text secret
	text="$(cat)"
	# Replace the known values first, including values that might not carry a
	# field label in a subprocess error.  They are random/hex in normal runs.
	for secret in "$ADMIN_PASSWORD" "$SESSION_SECRET" "$REGISTRATION_TOKEN" "$SECRET_KEY" "$ACCESS_KEY"; do
		[[ -n "$secret" ]] && text="${text//${secret}/[REDACTED]}"
	done
	printf '%s' "$text" | python3 -c '
import re, sys
text = sys.stdin.read()
field = r"(?:registration_token|secret_key|authorization|password|cookie|token|secret)"
# JSON/YAML quoted values.
text = re.sub(r"(?i)([\"\x27]?" + field + r"[\"\x27]?\s*:\s*)([\"\x27])(?:\\.|(?!\2).)*\2", r"\1\2[REDACTED]\2", text)
# Unquoted key/value or header diagnostics.
text = re.sub(r"(?i)(\b" + field + r"\b\s*[:=]\s*)(?!\[REDACTED\])[^,;\s}]+", r"\1[REDACTED]", text)
# Header-style Authorization/Cookie lines can contain several space-separated
# fields (for example Credential=...); redact the whole value, not just its
# first token.
text = re.sub(r"(?im)(\b(?:authorization|cookie)\b\s*[:=]\s*)[^\r\n]+", r"\1[REDACTED]", text)
# Query strings and SigV4 material.
text = re.sub(r"(?i)([?&](?:x-amz-[^=&\s]+|signature|token|secret|password|cookie|authorization)=)[^&\s]*", r"\1[REDACTED]", text)
text = re.sub(r"(?i)AWS4-HMAC-SHA256[^\s\"\x27]*", "AWS4-HMAC-SHA256[REDACTED]", text)
sys.stdout.write(text)
'
}

record() {
	REPORT_LINES+=("$*")
}

write_report() {
	[[ -n "$REPORT_PATH" ]] || return 0
	mkdir -p "$(dirname "$REPORT_PATH")"
	{
		printf 'panel-node-e2e mode=%s\n' "$MODE"
		printf '%s\n' "${REPORT_LINES[@]}"
	} | redact_text >"$REPORT_PATH"
	chmod 600 "$REPORT_PATH" 2>/dev/null || true
}

fail() {
	local message
	message="$(printf '%s' "$*" | redact_text)"
	record "FAIL: $message"
	collect_diagnostics || true
	printf 'panel-node-e2e failed: %s\n' "$message" >&2
	write_report || true
	exit 1
}

stop_process() {
	local pid="${1:-}"
	[[ -n "$pid" ]] || return 0
	if kill -0 "$pid" >/dev/null 2>&1; then
		kill -TERM "$pid" >/dev/null 2>&1 || true
		for _ in $(seq 1 80); do
			if ! kill -0 "$pid" >/dev/null 2>&1; then
				wait "$pid" >/dev/null 2>&1 || true
				return 0
			fi
			sleep 0.1
		done
		kill -KILL "$pid" >/dev/null 2>&1 || true
		wait "$pid" >/dev/null 2>&1 || true
	fi
}

cleanup() {
	local status=$?
	set +e
	if ((status != 0)); then
		# Catch set -e failures that bypass fail(), while the process logs and
		# temporary paths are still available for a redacted report.
		collect_diagnostics || true
	fi
	stop_process "$WRONG_NODE_PID"
	stop_process "$NODE_PID"
	stop_process "$PANEL_PID"
	if command -v docker >/dev/null 2>&1; then
		[[ -n "$WRONG_NODE_CONTAINER" ]] && docker rm -f "$WRONG_NODE_CONTAINER" >/dev/null 2>&1 || true
		[[ -n "$NODE_CONTAINER" ]] && docker rm -f "$NODE_CONTAINER" >/dev/null 2>&1 || true
		[[ -n "$PANEL_CONTAINER" ]] && docker rm -f "$PANEL_CONTAINER" >/dev/null 2>&1 || true
		[[ -n "$DOCKER_NETWORK" ]] && docker network rm "$DOCKER_NETWORK" >/dev/null 2>&1 || true
		[[ -n "$PANEL_IMAGE" ]] && docker image rm "$PANEL_IMAGE" >/dev/null 2>&1 || true
		[[ -n "$NODE_IMAGE" ]] && docker image rm "$NODE_IMAGE" >/dev/null 2>&1 || true
	fi
	[[ -n "$TMP_DIR" ]] && rm -rf "$TMP_DIR"
	write_report || true
	if [[ -n "$REPORT_PATH" && -f "$REPORT_PATH" ]]; then
		local secret
		for secret in "$ADMIN_PASSWORD" "$SESSION_SECRET" "$REGISTRATION_TOKEN" "$SECRET_KEY" "$ACCESS_KEY"; do
			# Scan every non-empty value, including short developer overrides.  A
			# report that may contain even a short password/token must fail closed.
			if [[ -n "$secret" ]] && grep -Fq -- "$secret" "$REPORT_PATH"; then
				rm -f "$REPORT_PATH"
				printf 'panel-node-e2e failed: redacted report contained sensitive material\n' >&2
				status=1
				break
			fi
		done
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pick_free_port() {
	python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    print(sock.getsockname()[1])
PY
}

json_field() {
	python3 - "$1" "$2" <<'PY'
import json, sys
value = json.load(open(sys.argv[1], encoding="utf-8"))
for part in sys.argv[2].split("."):
    value = value[part]
if isinstance(value, bool):
    print("true" if value else "false")
elif value is not None:
    print(value)
PY
}

json_has() {
	python3 - "$1" "$2" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
mode = sys.argv[2]
if mode == "service-mode":
    ok = obj.get("service_mode") == "panel"
elif mode == "synced":
    desired = obj.get("desired_version")
    applied = obj.get("applied_version")
    ok = bool(obj.get("online")) and obj.get("sync_state") == "synced"
    ok = ok and bool(obj.get("last_heartbeat"))
    ok = ok and isinstance(desired, int) and desired > 0 and desired == applied
else:
    ok = False
raise SystemExit(0 if ok else 1)
PY
}

safe_tail() {
	local file="$1"
	[[ -f "$file" ]] || return 0
	tail -n 40 "$file" 2>/dev/null | redact_text || true
}

collect_diagnostics() {
	[[ -n "$TMP_DIR" && -d "$TMP_DIR" ]] || return 0
	((DIAGNOSTICS_COLLECTED == 0)) || return 0
	DIAGNOSTICS_COLLECTED=1
	local file tail_text
	for file in \
		"$TMP_DIR/panel-stdout.log" "$TMP_DIR/node-stdout.log" "$TMP_DIR/wrong-node-stdout.log" \
		"$PANEL_LOG" "$NODE_LOG" "$WRONG_NODE_LOG" \
		"$TMP_DIR/browser-report.json"; do
		[[ -f "$file" ]] || continue
		tail_text="$(safe_tail "$file")"
		[[ -n "$tail_text" ]] && record "diagnostic $(basename "$file"): $tail_text"
	done
	# The browser helper writes a structured, redacted JSON report.  Keep that
	# report as the single browser diagnostic; its stderr is only a fallback for
	# failures that happen before the helper can create the report.
	if [[ ! -f "$TMP_DIR/browser-report.json" && -f "$TMP_DIR/browser.stderr" ]]; then
		tail_text="$(safe_tail "$TMP_DIR/browser.stderr")"
		[[ -n "$tail_text" ]] && record "diagnostic browser.stderr: $tail_text"
	fi
	if [[ "$MODE" == docker ]] && command -v docker >/dev/null 2>&1; then
		for container in "$PANEL_CONTAINER" "$NODE_CONTAINER" "$WRONG_NODE_CONTAINER"; do
			[[ -n "$container" ]] || continue
			if docker container inspect "$container" >/dev/null 2>&1; then
				tail_text="$(docker logs --tail 40 "$container" 2>&1 | redact_text || true)"
				[[ -n "$tail_text" ]] && record "diagnostic $container: $tail_text"
			fi
		done
	fi
}

require_commands() {
	local command_name
	for command_name in curl openssl python3 cmp sha256sum; do
		command -v "$command_name" >/dev/null 2>&1 || fail "required command is missing: $command_name"
	done
	if [[ "$SKIP_BUILD" != 1 ]]; then
		command -v go >/dev/null 2>&1 || fail 'required command is missing: go'
	fi
	if [[ "$MODE" == docker ]]; then
		command -v docker >/dev/null 2>&1 || fail 'Docker mode requires docker'
		docker info >/dev/null 2>&1 || fail 'Docker daemon is unavailable'
		docker compose version >/dev/null 2>&1 || fail 'Docker mode requires Docker Compose v2'
	fi
}

build_artifacts() {
	if [[ "$MODE" == docker ]]; then
		PANEL_IMAGE="natives3-e2e-panel-$E2E_RUN_ID"
		NODE_IMAGE="natives3-e2e-node-$E2E_RUN_ID"
		docker compose -f docker-compose.panel.yml config >/dev/null
		docker compose -f docker-compose.node.yml config >/dev/null
		record 'Panel/Node Compose templates validated'
		printf 'building final Panel and Node Docker targets\n'
		docker build --target panel -t "$PANEL_IMAGE" . >/dev/null
		docker build --target node -t "$NODE_IMAGE" . >/dev/null
		record 'Dockerfile panel/node final targets built'
		return
	fi
	if [[ "$SKIP_BUILD" == 1 ]]; then
		PANEL_BIN="${E2E_PANEL_BIN:-$ROOT_DIR/.e2e-panel}"
		NODE_BIN="${E2E_NODE_BIN:-$ROOT_DIR/.e2e-node}"
		[[ -x "$PANEL_BIN" && -x "$NODE_BIN" ]] || fail '--skip-build requires executable E2E_PANEL_BIN and E2E_NODE_BIN'
		return
	fi
	command -v npm >/dev/null 2>&1 || fail 'local mode requires npm for the embedded Panel UI'
	printf 'building embedded Panel UI\n'
	(
		cd pkg/webadmin/ui
		npm ci --no-audit --no-fund
		npm run build
	)
	PANEL_BIN="$TMP_DIR/panel"
	NODE_BIN="$TMP_DIR/node"
	printf 'building Panel and Node binaries\n'
	GOWORK=off go build -o "$PANEL_BIN" ./cmd/panel
	GOWORK=off go build -o "$NODE_BIN" ./cmd/node
	record 'local Panel/Node binaries built'
}

generate_pki() {
	local san='DNS:localhost,IP:127.0.0.1'
	[[ "$MODE" == docker ]] && san+=',DNS:panel'
	mkdir -p "$TMP_DIR/pki" "$TMP_DIR/secrets"
	head -c 32 /dev/urandom >"$TMP_DIR/secrets/master.key"
	chmod 600 "$TMP_DIR/secrets/master.key"
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$TMP_DIR/pki/intermediate.key" >/dev/null 2>&1
	openssl req -x509 -new -key "$TMP_DIR/pki/intermediate.key" -sha256 -days 2 \
		-subj '/CN=NativeS3 E2E Intermediate CA' \
		-addext 'basicConstraints=critical,CA:TRUE,pathlen:1' \
		-addext 'keyUsage=critical,keyCertSign,cRLSign' \
		-out "$TMP_DIR/pki/intermediate.crt" >/dev/null 2>&1
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$TMP_DIR/pki/panel-server.key" >/dev/null 2>&1
	openssl req -new -key "$TMP_DIR/pki/panel-server.key" -subj '/CN=panel' \
		-addext "subjectAltName=$san" -out "$TMP_DIR/pki/panel-server.csr" >/dev/null 2>&1
	printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$san" >"$TMP_DIR/pki/server.ext"
	openssl x509 -req -in "$TMP_DIR/pki/panel-server.csr" \
		-CA "$TMP_DIR/pki/intermediate.crt" -CAkey "$TMP_DIR/pki/intermediate.key" -CAcreateserial \
		-days 2 -sha256 -extfile "$TMP_DIR/pki/server.ext" -out "$TMP_DIR/pki/panel-server.crt" >/dev/null 2>&1
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$TMP_DIR/pki/wrong-ca.key" >/dev/null 2>&1
	openssl req -x509 -new -key "$TMP_DIR/pki/wrong-ca.key" -sha256 -days 2 \
		-subj '/CN=Wrong E2E CA' -addext 'basicConstraints=critical,CA:TRUE' \
		-addext 'keyUsage=critical,keyCertSign,cRLSign' -out "$TMP_DIR/pki/wrong-ca.crt" >/dev/null 2>&1
	record 'temporary private PKI generated'
}

prepare_data_roots() {
	mkdir -p \
		"$TMP_DIR/panel-data/pki" "$TMP_DIR/panel-data/secrets" "$TMP_DIR/panel-data/logs" \
		"$TMP_DIR/node-data/pki" "$TMP_DIR/node-data/objects" "$TMP_DIR/node-data/logs" \
		"$TMP_DIR/wrong-node-data/pki" "$TMP_DIR/wrong-node-data/objects" "$TMP_DIR/wrong-node-data/logs"
	cp "$TMP_DIR/pki/panel-server.crt" "$TMP_DIR/panel-data/pki/panel-server.crt"
	cp "$TMP_DIR/pki/panel-server.key" "$TMP_DIR/panel-data/pki/panel-server.key"
	cp "$TMP_DIR/pki/intermediate.crt" "$TMP_DIR/panel-data/pki/intermediate.crt"
	cp "$TMP_DIR/pki/intermediate.key" "$TMP_DIR/panel-data/pki/intermediate.key"
	cp "$TMP_DIR/secrets/master.key" "$TMP_DIR/panel-data/secrets/master.key"
	cp "$TMP_DIR/pki/intermediate.crt" "$TMP_DIR/node-data/pki/panel-ca.crt"
	cp "$TMP_DIR/pki/wrong-ca.crt" "$TMP_DIR/wrong-node-data/pki/wrong-ca.crt"
	chmod 600 "$TMP_DIR/panel-data/secrets/master.key" "$TMP_DIR/panel-data/pki/"*.key
}

write_configs() {
	local token="${1:-}"
	local panel_root node_root wrong_root admin_bind agent_bind s3_bind wrong_s3_bind register_url agent_url
	PANEL_CONFIG="$TMP_DIR/panel.yaml"
	NODE_CONFIG="$TMP_DIR/node.yaml"
	WRONG_NODE_CONFIG="$TMP_DIR/wrong-node.yaml"
	if [[ "$MODE" == docker ]]; then
		panel_root=/data
		node_root=/data
		wrong_root=/data
		admin_bind='0.0.0.0:9001'
		agent_bind='0.0.0.0:9443'
		s3_bind='0.0.0.0:9000'
		wrong_s3_bind='0.0.0.0:9000'
		register_url='https://panel:9443/register'
		agent_url='wss://panel:9443/agent'
		PANEL_LOG="$TMP_DIR/panel-data/logs/panel.log"
		NODE_LOG="$TMP_DIR/node-data/logs/node.log"
		WRONG_NODE_LOG="$TMP_DIR/wrong-node-data/logs/node.log"
	else
		panel_root="$TMP_DIR/panel-data"
		node_root="$TMP_DIR/node-data"
		wrong_root="$TMP_DIR/wrong-node-data"
		admin_bind="127.0.0.1:$PANEL_ADMIN_PORT"
		agent_bind="127.0.0.1:$PANEL_AGENT_PORT"
		s3_bind="127.0.0.1:$NODE_S3_PORT"
		wrong_s3_bind="127.0.0.1:$WRONG_NODE_S3_PORT"
		register_url="https://127.0.0.1:$PANEL_AGENT_PORT/register"
		agent_url="wss://127.0.0.1:$PANEL_AGENT_PORT/agent"
		PANEL_LOG="$TMP_DIR/panel-data/logs/panel.log"
		NODE_LOG="$TMP_DIR/node-data/logs/node.log"
		WRONG_NODE_LOG="$TMP_DIR/wrong-node-data/logs/node.log"
	fi
	cat >"$PANEL_CONFIG" <<EOF
admin_addr: "$admin_bind"
agent:
  addr: "$agent_bind"
  cert_file: "$panel_root/pki/panel-server.crt"
  key_file: "$panel_root/pki/panel-server.key"
pki:
  intermediate_cert_file: "$panel_root/pki/intermediate.crt"
  intermediate_key_file: "$panel_root/pki/intermediate.key"
  client_cert_ttl: 24h
master_key_file: "$panel_root/secrets/master.key"
database:
  driver: sqlite
  dsn: "$panel_root/panel.db"
webadmin:
  admin_bootstrap_password: "$ADMIN_PASSWORD"
  session_secret: "$SESSION_SECRET"
  session_ttl_minutes: 60
heartbeat_interval: 1s
offline_multiplier: 3
log_level: info
log:
  file: "$panel_root/logs/panel.log"
  max_size_mb: 5
  max_backups: 1
EOF
	cat >"$NODE_CONFIG" <<EOF
server:
  s3_addr: "$s3_bind"
storage:
  data_root: "$node_root/objects"
  metadata_suffix: ".s3meta"
database:
  driver: sqlite
  dsn: "$node_root/node.db"
region: us-east-1
log_level: info
log:
  file: "$node_root/logs/node.log"
  max_size_mb: 5
  max_backups: 1
panel:
  node_id: 1
  register_url: "$register_url"
  agent_url: "$agent_url"
  registration_token: "$token"
  cert_file: "$node_root/pki/node.crt"
  key_file: "$node_root/pki/node.key"
  ca_file: "$node_root/pki/panel-ca.crt"
  heartbeat_interval: 1s
EOF
	cat >"$WRONG_NODE_CONFIG" <<EOF
server:
  s3_addr: "$wrong_s3_bind"
storage:
  data_root: "$wrong_root/objects"
database:
  driver: sqlite
  dsn: "$wrong_root/node.db"
region: us-east-1
log_level: info
log:
  file: "$wrong_root/logs/node.log"
  max_size_mb: 5
  max_backups: 1
panel:
  node_id: 1
  register_url: "$register_url"
  agent_url: "$agent_url"
  registration_token: "invalid-e2e-token"
  cert_file: "$wrong_root/pki/node.crt"
  key_file: "$wrong_root/pki/node.key"
  ca_file: "$wrong_root/pki/wrong-ca.crt"
  heartbeat_interval: 1s
EOF
	# Keep configs private. Docker runs the final image with the invoking UID so
	# this mode-600 bind mount remains readable without weakening host exposure.
	chmod 600 "$PANEL_CONFIG" "$NODE_CONFIG" "$WRONG_NODE_CONFIG"
}

start_panel() {
	if [[ "$MODE" == docker ]]; then
		if docker container inspect "$PANEL_CONTAINER" >/dev/null 2>&1; then
			docker start "$PANEL_CONTAINER" >/dev/null
		else
			docker run -d --name "$PANEL_CONTAINER" --user "$DOCKER_USER" \
				--network "$DOCKER_NETWORK" --network-alias panel \
				-p "127.0.0.1:$PANEL_ADMIN_PORT:9001" \
				-p "127.0.0.1:$PANEL_AGENT_PORT:9443" \
				-v "$TMP_DIR/panel-data:/data" \
				-v "$PANEL_CONFIG:/app/configs/panel.yaml:ro" \
				"$PANEL_IMAGE" -config /app/configs/panel.yaml >/dev/null
		fi
		return 0
	fi
	"$PANEL_BIN" -config "$PANEL_CONFIG" >"$TMP_DIR/panel-stdout.log" 2>&1 &
	PANEL_PID=$!
}

stop_panel() {
	if [[ "$MODE" == docker ]]; then
		[[ -n "$PANEL_CONTAINER" ]] && docker stop "$PANEL_CONTAINER" >/dev/null 2>&1 || true
		return 0
	fi
	if [[ -n "$PANEL_PID" ]]; then
		stop_process "$PANEL_PID"
		PANEL_PID=""
	fi
}

start_node() {
	if [[ "$MODE" == docker ]]; then
		if docker container inspect "$NODE_CONTAINER" >/dev/null 2>&1; then
			docker start "$NODE_CONTAINER" >/dev/null
		else
			docker run -d --name "$NODE_CONTAINER" --user "$DOCKER_USER" --network "$DOCKER_NETWORK" \
				-p "127.0.0.1:$NODE_S3_PORT:9000" \
				-v "$TMP_DIR/node-data:/data" \
				-v "$NODE_CONFIG:/app/configs/node.yaml:ro" \
				"$NODE_IMAGE" -config /app/configs/node.yaml >/dev/null
		fi
		return 0
	fi
	"$NODE_BIN" -config "$NODE_CONFIG" >"$TMP_DIR/node-stdout.log" 2>&1 &
	NODE_PID=$!
}

stop_node() {
	if [[ "$MODE" == docker ]]; then
		[[ -n "$NODE_CONTAINER" ]] && docker stop "$NODE_CONTAINER" >/dev/null 2>&1 || true
		return 0
	fi
	if [[ -n "$NODE_PID" ]]; then
		stop_process "$NODE_PID"
		NODE_PID=""
	fi
}

start_wrong_node() {
	if [[ "$MODE" == docker ]]; then
		docker run -d --name "$WRONG_NODE_CONTAINER" --user "$DOCKER_USER" --network "$DOCKER_NETWORK" \
			-p "127.0.0.1:$WRONG_NODE_S3_PORT:9000" \
			-v "$TMP_DIR/wrong-node-data:/data" \
			-v "$WRONG_NODE_CONFIG:/app/configs/node.yaml:ro" \
			"$NODE_IMAGE" -config /app/configs/node.yaml >/dev/null
		return 0
	fi
	"$NODE_BIN" -config "$WRONG_NODE_CONFIG" >"$TMP_DIR/wrong-node-stdout.log" 2>&1 &
	WRONG_NODE_PID=$!
}

stop_wrong_node() {
	if [[ "$MODE" == docker ]]; then
		[[ -n "$WRONG_NODE_CONTAINER" ]] && docker rm -f "$WRONG_NODE_CONTAINER" >/dev/null 2>&1 || true
		return 0
	fi
	if [[ -n "$WRONG_NODE_PID" ]]; then
		stop_process "$WRONG_NODE_PID"
		WRONG_NODE_PID=""
	fi
}

prepare_docker() {
	docker network create "$DOCKER_NETWORK" >/dev/null
}

wait_panel_ready() {
	local deadline=$((SECONDS + TIMEOUT)) body="$TMP_DIR/panel-ready.json" status
	while ((SECONDS < deadline)); do
		status="$(curl -sS --connect-timeout 2 --max-time 4 \
			-o "$body" -w '%{http_code}' "$PANEL_ADMIN_URL/api/admin/auth-settings" \
			2>"$TMP_DIR/panel-ready.err" || true)"
		if [[ "$status" == 200 ]] && json_has "$body" service-mode; then
			record 'Panel readiness at /api/admin/auth-settings'
			return 0
		fi
		if [[ "$MODE" == local && -n "$PANEL_PID" ]] && ! kill -0 "$PANEL_PID" >/dev/null 2>&1; then
			fail "Panel exited before readiness: $(safe_tail "$TMP_DIR/panel-stdout.log")"
		fi
		sleep 0.2
	done
	fail 'timed out waiting for Panel /api/admin/auth-settings'
}

wait_s3() {
	local url="$1" label="$2" deadline=$((SECONDS + TIMEOUT)) status
	while ((SECONDS < deadline)); do
		status="$(curl -sS --connect-timeout 2 --max-time 3 -o /dev/null -w '%{http_code}' \
			"$url/" 2>/dev/null || true)"
		if [[ "$status" =~ ^[1-5][0-9][0-9]$ ]]; then
			record "$label S3 listener reachable (HTTP $status)"
			return 0
		fi
		sleep 0.2
	done
	fail "timed out waiting for $label S3 listener"
}

api_request() {
	local method="$1" path="$2" body_file="${3:-}"
	local out="$TMP_DIR/api-$RANDOM-$RANDOM.json" err="$TMP_DIR/api-$RANDOM-$RANDOM.err"
	local -a args=(curl -sS --connect-timeout 3 --max-time 15 \
		-H 'Accept: application/json' -b "$COOKIE_FILE" -c "$COOKIE_FILE" -X "$method")
	if [[ -n "$body_file" ]]; then
		args+=( -H 'Content-Type: application/json' --data-binary "@$body_file" )
	fi
	API_STATUS="$("${args[@]}" -o "$out" -w '%{http_code}' "$PANEL_ADMIN_URL$path" \
		2>"$err" || true)"
	API_BODY="$out"
	API_ERROR="$(redact_text <"$err" 2>/dev/null || true)"
}

api_expect() {
	local expected="$1" method="$2" path="$3" body_file="${4:-}"
	api_request "$method" "$path" "$body_file"
	if [[ "$API_STATUS" != "$expected" ]]; then
		fail "Panel API $method $path returned HTTP ${API_STATUS:-000}${API_ERROR:+: $API_ERROR}"
	fi
}

admin_login() {
	local login_body="$TMP_DIR/login.json"
	rm -f "$COOKIE_FILE"
	E2E_ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 - "$login_body" <<'PY'
import json, os, sys
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    json.dump({"password": os.environ["E2E_ADMIN_PASSWORD"]}, handle)
    handle.write("\n")
PY
	chmod 600 "$login_body"
	api_expect 200 POST /api/admin/login "$login_body"
	record 'Panel Admin API login (cookie retained only in temp jar)'
}

s3_request() {
	local method="$1" path="$2" out="$3" body_file="${4:-}"
	local err="$TMP_DIR/s3-$RANDOM-$RANDOM.err"
	local -a args=(curl -sS --connect-timeout 3 --max-time 15 \
		--aws-sigv4 "aws:amz:${AWS_REGION}:s3" \
		--user "$ACCESS_KEY:$SECRET_KEY" -X "$method")
	if [[ -n "$body_file" ]]; then
		args+=(--upload-file "$body_file")
	fi
	S3_STATUS="$("${args[@]}" -o "$out" -w '%{http_code}' "$NODE_S3_URL$path" \
		2>"$err" || true)"
	S3_ERROR="$(redact_text <"$err" 2>/dev/null || true)"
}

s3_expect() {
	local expected="$1" method="$2" path="$3" out="$4" body_file="${5:-}"
	s3_request "$method" "$path" "$out" "$body_file"
	if [[ "$S3_STATUS" != "$expected" ]]; then
		fail "S3 $method $path returned HTTP ${S3_STATUS:-000}${S3_ERROR:+: $S3_ERROR}"
	fi
}

poll_node_synced() {
	local deadline=$((SECONDS + TIMEOUT))
	while ((SECONDS < deadline)); do
		api_request GET /api/admin/nodes/1
		if [[ "$API_STATUS" == 200 ]] && json_has "$API_BODY" synced; then
			record 'Node online + heartbeat + desired/applied synced'
			return 0
		fi
		sleep 0.3
	done
	fail 'Node did not reach online/synced state'
}

run_browser_gate() {
	if [[ "$SKIP_BROWSER" == 1 ]]; then
		record 'browser gate skipped by request'
		return 0
	fi
	local browser_report="$TMP_DIR/browser-report.json"
	E2E_ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 scripts/internal/e2e-browser.py \
		--panel-url "$PANEL_ADMIN_URL" \
		--expected-node-name "$NODE_NAME" \
		--timeout "$TIMEOUT" \
		--report "$browser_report" \
		>"$TMP_DIR/browser.stdout" 2>"$TMP_DIR/browser.stderr" || {
			fail "browser gate failed: $(redact_text <"$TMP_DIR/browser.stderr" 2>/dev/null || true)"
		}
	record 'ChromeDriver Panel /dashboard -> /nodes and API boundary assertions'
}

main() {
	require_commands
	TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/natives3-panel-node-e2e.XXXXXX")"
	chmod 700 "$TMP_DIR"
	DOCKER_USER="$(id -u):$(id -g)"
	E2E_RUN_ID="$(date +%s)-$RANDOM"
	ADMIN_PASSWORD="${E2E_ADMIN_PASSWORD:-$(openssl rand -hex 18)}"
	SESSION_SECRET="$(openssl rand -hex 32)"
	AWS_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
	PANEL_ADMIN_PORT="$(pick_free_port)"
	PANEL_AGENT_PORT="$(pick_free_port)"
	NODE_S3_PORT="$(pick_free_port)"
	WRONG_NODE_S3_PORT="$(pick_free_port)"
	PANEL_ADMIN_URL="http://127.0.0.1:$PANEL_ADMIN_PORT"
	NODE_S3_URL="http://127.0.0.1:$NODE_S3_PORT"
	COOKIE_FILE="$TMP_DIR/admin.cookies"
	PANEL_CONTAINER="natives3-e2e-panel-$E2E_RUN_ID"
	NODE_CONTAINER="natives3-e2e-node-$E2E_RUN_ID"
	WRONG_NODE_CONTAINER="natives3-e2e-wrong-node-$E2E_RUN_ID"
	DOCKER_NETWORK="natives3-e2e-net-$E2E_RUN_ID"
	NODE_NAME="e2e-node-${E2E_RUN_ID//[^a-zA-Z0-9-]/-}"

	generate_pki
	prepare_data_roots
	build_artifacts
	[[ "$MODE" == docker ]] && prepare_docker
	write_configs

	if [[ "$MODE" == local ]]; then
		"$PANEL_BIN" -check-config -config "$PANEL_CONFIG" >/dev/null
		"$NODE_BIN" -check-config -config "$NODE_CONFIG" >/dev/null
	fi
	start_panel
	wait_panel_ready
	admin_login

	# Keep Node offline while Panel creates the authoritative draft and publishes
	# an immutable desired snapshot.
	local request_file
	request_file="$TMP_DIR/create-node.json"
	printf '{"display_name":"%s"}\n' "$NODE_NAME" >"$request_file"
	api_expect 201 POST /api/admin/nodes "$request_file"
	[[ "$(json_field "$API_BODY" id)" == 1 ]] || fail 'first Panel node did not receive id 1'
	api_expect 201 POST /api/admin/nodes/1/tokens
	REGISTRATION_TOKEN="$(json_field "$API_BODY" token)"
	[[ -n "$REGISTRATION_TOKEN" ]] || fail 'Panel returned an empty registration token'

	request_file="$TMP_DIR/create-bucket.json"
	BUCKET_NAME="e2e-bucket-${E2E_RUN_ID//[^a-zA-Z0-9-]/-}"
	BUCKET_NAME="${BUCKET_NAME,,}"
	printf '{"name":"%s","acl":"private"}\n' "$BUCKET_NAME" >"$request_file"
	api_expect 201 POST /api/admin/nodes/1/buckets "$request_file"
	request_file="$TMP_DIR/create-credential.json"
	printf '{"name":"e2e-credential","bucket":"%s","quota_bytes":0}\n' "$BUCKET_NAME" >"$request_file"
	api_expect 201 POST /api/admin/nodes/1/credentials "$request_file"
	ACCESS_KEY="$(json_field "$API_BODY" access_key)"
	SECRET_KEY="$(json_field "$API_BODY" secret_key)"
	[[ -n "$ACCESS_KEY" && -n "$SECRET_KEY" ]] || fail 'Panel credential response omitted one-time secret'
	api_expect 200 POST /api/admin/nodes/1/desired-state
	record 'Panel node/token/bucket/credential creation and desired-state publish'

	# Inject the one-time token into the mounted config only after the API setup;
	# it never appears in output.  The node writes its key/cert under node-data.
	write_configs "$REGISTRATION_TOKEN"
	start_node
	wait_s3 "$NODE_S3_URL" node
	poll_node_synced
	[[ -s "$TMP_DIR/node-data/pki/node.crt" && -s "$TMP_DIR/node-data/pki/node.key" ]] || \
		fail 'Node registration did not persist its mTLS identity'
	if [[ "$MODE" == docker ]]; then
		panel_ports="$(docker port "$PANEL_CONTAINER")"
		node_ports="$(docker port "$NODE_CONTAINER")"
		grep -q '^9001/tcp' <<<"$panel_ports" || fail 'Panel container did not map admin port 9001'
		grep -q '^9443/tcp' <<<"$panel_ports" || fail 'Panel container did not map agent port 9443'
		! grep -q '^9000/tcp' <<<"$panel_ports" || fail 'Panel container exposed S3 port 9000'
		grep -q '^9000/tcp' <<<"$node_ports" || fail 'Node container did not map S3 port 9000'
		! grep -Eq '^9001/tcp|^9443/tcp' <<<"$node_ports" || fail 'Node container exposed an admin/control port'
		record 'Docker Panel/Node port boundary'
	fi

	local empty_file="$TMP_DIR/empty" object_src="$TMP_DIR/object.txt" object_dst="$TMP_DIR/object.out"
	local survivor_src="$TMP_DIR/survivor.txt" survivor_dst="$TMP_DIR/survivor.out"
	local native_file="$TMP_DIR/node-data/objects/$BUCKET_NAME/e2e/object.txt"
	: >"$empty_file"
	s3_request PUT "/$BUCKET_NAME" "$TMP_DIR/bucket-create.out" "$empty_file"
	[[ "$S3_STATUS" == 403 ]] || fail "managed direct bucket create returned HTTP ${S3_STATUS:-000}, want 403"
	s3_expect 200 HEAD "/$BUCKET_NAME" "$TMP_DIR/bucket-head.out"
	printf 'panel-node-e2e payload\n' >"$object_src"
	s3_expect 200 PUT "/$BUCKET_NAME/e2e/object.txt" "$TMP_DIR/put.out" "$object_src"
	s3_expect 200 HEAD "/$BUCKET_NAME/e2e/object.txt" "$TMP_DIR/object-head.out"
	s3_expect 200 GET "/$BUCKET_NAME/e2e/object.txt" "$object_dst"
	cmp "$object_src" "$object_dst" || fail 'S3 GET bytes differ from PUT bytes'
	[[ -f "$native_file" ]] || fail 'native object file was not written'
	cmp "$object_src" "$native_file" || fail 'native object bytes differ from HTTP payload'
	s3_expect 204 DELETE "/$BUCKET_NAME/e2e/object.txt" "$TMP_DIR/delete.out"
	[[ ! -e "$native_file" ]] || fail 'native object remained after DELETE'
	printf 'survives panel and node restart\n' >"$survivor_src"
	s3_expect 200 PUT "/$BUCKET_NAME/e2e/survivor.txt" "$TMP_DIR/survivor-put.out" "$survivor_src"
	record 'SigV4 bucket confirmation and object PUT/HEAD/GET/DELETE byte checks'

	# Panel outage: Node keeps serving its S3 listener and reconnects after the
	# same Panel data/PKI comes back.  Re-login explicitly because sessions are
	# process memory and are intentionally not persisted.
	stop_panel
	sleep 1
	wait_s3 "$NODE_S3_URL" node-during-panel-outage
	s3_expect 200 GET "/$BUCKET_NAME/e2e/survivor.txt" "$survivor_dst"
	cmp "$survivor_src" "$survivor_dst" || fail 'S3 bytes changed during Panel outage'
	if [[ "$MODE" == local ]]; then
		kill -0 "$NODE_PID" >/dev/null 2>&1 || fail 'Node exited while Panel was stopped'
	else
		docker container inspect -f '{{.State.Running}}' "$NODE_CONTAINER" | grep -qx true || fail 'Node stopped while Panel was stopped'
	fi
	start_panel
	wait_panel_ready
	admin_login
	poll_node_synced
	record 'Panel outage/restart recovery and re-login'

	# Restart Node after removing its token.  The persisted key/certificate must
	# be reused; no second registration token is issued or consumed.
	local key_before key_after
	key_before="$(sha256sum "$TMP_DIR/node-data/pki/node.key" | awk '{print $1}')"
	stop_node
	write_configs ''
	start_node
	wait_s3 "$NODE_S3_URL" node-after-restart
	poll_node_synced
	s3_expect 200 GET "/$BUCKET_NAME/e2e/survivor.txt" "$survivor_dst"
	cmp "$survivor_src" "$survivor_dst" || fail 'S3 bytes changed across Node restart'
	key_after="$(sha256sum "$TMP_DIR/node-data/pki/node.key" | awk '{print $1}')"
	[[ "$key_before" == "$key_after" ]] || fail 'Node private key changed across restart'
	record 'Node restart reused persisted mTLS identity without token'

	# Negative CA check: a fresh node with an unrelated trust anchor never gets a
	# client certificate, yet its independent S3 process remains alive.
	start_wrong_node
	wait_s3 "http://127.0.0.1:$WRONG_NODE_S3_PORT" wrong-ca-node
	sleep 2
	if [[ "$MODE" == local ]]; then
		kill -0 "$WRONG_NODE_PID" >/dev/null 2>&1 || fail 'wrong-CA Node exited instead of serving S3'
	else
		docker container inspect -f '{{.State.Running}}' "$WRONG_NODE_CONTAINER" | grep -qx true || fail 'wrong-CA Node container exited'
	fi
	[[ ! -e "$TMP_DIR/wrong-node-data/pki/node.crt" ]] || fail 'wrong-CA Node unexpectedly persisted a client certificate'
	if ! grep -Eiq 'x509|certificate[^[:cntrl:]]*(unknown|verify)|tls[^[:cntrl:]]*bad certificate' \
		"$WRONG_NODE_LOG" "$TMP_DIR/wrong-node-stdout.log" \
		"$PANEL_LOG" "$TMP_DIR/panel-stdout.log" 2>/dev/null; then
		fail 'wrong-CA Node did not report a TLS certificate verification failure'
	fi
	record 'wrong-CA registration failed closed while S3 stayed reachable'
	stop_wrong_node
	s3_expect 204 DELETE "/$BUCKET_NAME/e2e/survivor.txt" "$TMP_DIR/survivor-delete.out"

	run_browser_gate
	record 'registration response-loss replay: pkg/nodeagent package regression evidence runs in quality gate'
	record 'PASS: Panel -> Node release gate complete'
	printf 'panel-node-e2e passed (%s mode)\n' "$MODE"
}

main "$@"
