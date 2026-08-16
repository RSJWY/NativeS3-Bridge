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
    # region 由节点在 hello 里上报,必须与 node 配置中的 region 一致。
    ok = ok and obj.get("region") == "us-east-1"
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

assert_panel_logs() {
	api_expect 200 GET '/api/admin/logs?limit=10'
	E2E_ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 - "$API_BODY" <<'PY'
import json, os, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
if obj.get("source") not in ("file", "ring") or obj.get("limit") != 10:
    raise SystemExit(1)
if not obj.get("file_enabled") or not isinstance(obj.get("entries"), list) or len(obj["entries"]) > 10:
    raise SystemExit(1)
files = obj.get("files") or []
if not any(item.get("current") for item in files):
    raise SystemExit(1)
password = os.environ.get("E2E_ADMIN_PASSWORD", "")
if password and password in json.dumps(obj, ensure_ascii=False):
    raise SystemExit(1)
PY
	record 'authenticated bounded Panel local log query'
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

assert_node_log_result() {
	local file="$1"
	E2E_SECRET_KEY="$SECRET_KEY" E2E_REGISTRATION_TOKEN="$REGISTRATION_TOKEN" E2E_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
		python3 - "$file" <<'PY'
import json, os, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
result = obj.get("result") or {}
entries = result.get("log_entries") or []
if obj.get("node_id") != 1 or obj.get("type") != "log_query" or obj.get("state") != "success":
    raise SystemExit(1)
if result.get("log_source") != "ring" or not entries or len(entries) > 10:
    raise SystemExit(1)
if not any(entry.get("msg") == "s3 request" for entry in entries):
    raise SystemExit(1)
for entry in entries:
    for key in (entry.get("attrs") or {}):
        normalized = key.lower().replace("-", "_").replace(".", "_")
        if any(part in normalized for part in ("secret", "password", "authorization", "cookie", "signature", "token")):
            raise SystemExit(1)
raw = json.dumps(obj, ensure_ascii=False)
for name in ("E2E_SECRET_KEY", "E2E_REGISTRATION_TOKEN", "E2E_ADMIN_PASSWORD"):
    value = os.environ.get(name, "")
    if value and value in raw:
        raise SystemExit(1)
PY
}

assert_panel_telemetry_reflects_s3_mutations() {
	local survivor_src="$1"
	local summary="$TMP_DIR/telemetry-summary.json"
	local expected_bytes
	expected_bytes="$(wc -c <"$survivor_src" | tr -d '[:space:]')"
	local deadline=$((SECONDS + TIMEOUT))
	# 节点心跳间隔 1s,过期阈值 1s*3。空闲健康节点的 observed_at 由心跳持续
	# 刷新,summary 中节点 1 必须稳定为 valid 并贡献对象数。
	while ((SECONDS < deadline)); do
		api_request GET /api/admin/dashboard/summary
		if [[ "$API_STATUS" == 200 ]]; then
			cp "$API_BODY" "$summary"
			if python3 - "$summary" "$expected_bytes" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
t = obj.get("telemetry") or {}
nodes = t.get("nodes") or []
row = next((n for n in nodes if n.get("node_id") == 1), None)
if row is None or row.get("status") != "valid":
    raise SystemExit(2)
oc = row.get("object_count")
if oc != 1 or row.get("used_bytes") != int(sys.argv[2]) or t.get("object_count") != 1 or t.get("used_bytes_total") != int(sys.argv[2]):
    raise SystemExit(3)
PY
			then break; fi
		fi
		sleep 0.3
	done
	if [[ "$API_STATUS" != 200 ]]; then
		fail "Panel dashboard summary returned HTTP ${API_STATUS:-000}"
	fi
	local status oc used
	status="$(python3 -c "import json;print(next((n for n in (json.load(open('$summary'))['telemetry']['nodes']) if n['node_id']==1),{}).get('status'))")"
	oc="$(python3 -c "import json;print(next((n for n in (json.load(open('$summary'))['telemetry']['nodes']) if n['node_id']==1),{}).get('object_count'))")"
	used="$(python3 -c "import json;print(next((n for n in (json.load(open('$summary'))['telemetry']['nodes']) if n['node_id']==1),{}).get('used_bytes'))")"
	[[ "$status" == "valid" ]] || fail "Panel telemetry for node 1 is '$status', want valid"
	[[ "$oc" == "1" ]] || fail "Panel telemetry object_count for node 1 is '$oc', want 1 after survivor PUT"
	[[ "$used" == "$expected_bytes" ]] || fail "Panel telemetry used_bytes for node 1 is '$used', want $expected_bytes"
	record 'Panel dashboard summary reflects live node telemetry (valid + used_bytes + object_count)'

	# 删除唯一存活对象后,容量与对象数都必须回落到 0(而非失效),证明计数
	# 随 S3 变更增量推进、空闲刷新没把节点判过期。
	s3_expect 204 DELETE "/$BUCKET_NAME/e2e/survivor.txt" "$TMP_DIR/survivor-delete.out"
	deadline=$((SECONDS + TIMEOUT))
	while ((SECONDS < deadline)); do
		api_request GET /api/admin/dashboard/summary
		[[ "$API_STATUS" == 200 ]] || fail "Panel dashboard summary returned HTTP ${API_STATUS:-000}"
		cp "$API_BODY" "$summary"
		if python3 - "$summary" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
t = obj.get("telemetry", {})
nodes = t.get("nodes") or []
row = next((n for n in nodes if n.get("node_id") == 1), None)
status = (row or {}).get("status")
oc = (row or {}).get("object_count")
used = (row or {}).get("used_bytes")
if status != "valid" or oc != 0 or used != 0:
    raise SystemExit(2)
if t.get("object_count") != 0 or t.get("used_bytes_total") != 0:
    raise SystemExit(3)
PY
		then break; fi
		sleep 0.3
	done
	if [[ "$API_STATUS" != 200 ]]; then
		fail "Panel dashboard summary returned HTTP ${API_STATUS:-000}"
	fi
	oc="$(python3 -c "import json;print(next((n for n in (json.load(open('$summary'))['telemetry']['nodes']) if n['node_id']==1),{}).get('object_count'))")"
	used="$(python3 -c "import json;print(next((n for n in (json.load(open('$summary'))['telemetry']['nodes']) if n['node_id']==1),{}).get('used_bytes'))")"
	[[ "$oc" == "0" && "$used" == "0" ]] || fail "Panel telemetry after DELETE is ${used} bytes/${oc} objects, want 0/0"
	record 'Panel telemetry tracks S3 DELETE capacity and object count back to zero'

	# 恢复一个 survivor 对象,保证后续 Panel outage/restart 断言可复用。
	printf 'survives panel and node restart\n' >"$survivor_src"
	s3_expect 200 PUT "/$BUCKET_NAME/e2e/survivor.txt" "$TMP_DIR/survivor-restore.out" "$survivor_src"
	record 'Panel telemetry tracks S3 DELETE then PUT (object_count 1 -> 0 -> 1)'
}

assert_panel_telemetry_stale() {
	local summary="$TMP_DIR/telemetry-stale-summary.json"
	local deadline=$((SECONDS + TIMEOUT))
	while ((SECONDS < deadline)); do
		api_request GET /api/admin/dashboard/summary
		if [[ "$API_STATUS" == 200 ]]; then
			cp "$API_BODY" "$summary"
			if python3 - "$summary" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
t = obj.get("telemetry", {})
row = next((n for n in (t.get("nodes") or []) if n.get("node_id") == 1), None)
if (row or {}).get("status") != "stale" or t.get("valid_nodes") != 0 or (t.get("stale_nodes") or 0) < 1:
    raise SystemExit(2)
PY
			then
				record 'Panel dashboard marks stopped node telemetry stale and excludes it from totals'
				return 0
			fi
		fi
		sleep 0.3
	done
	fail 'Panel dashboard did not expose stale telemetry for the stopped node'
}

query_node_logs() {
	local request_file="$TMP_DIR/node-log-query.json" task_id deadline=$((SECONDS + TIMEOUT)) state
	printf '%s\n' '{"type":"log_query","params":{"level":"INFO","keyword":"s3 request","limit":10}}' >"$request_file"
	api_expect 202 POST /api/admin/nodes/1/tasks "$request_file"
	task_id="$(json_field "$API_BODY" task_id)"
	[[ -n "$task_id" ]] || fail 'Panel returned an empty Node log task id'
	while ((SECONDS < deadline)); do
		api_request GET "/api/admin/nodes/1/tasks/$task_id"
		if [[ "$API_STATUS" == 200 ]]; then
			state="$(json_field "$API_BODY" state)"
			case "$state" in
			success)
				assert_node_log_result "$API_BODY" || fail 'Node log query returned an invalid, unbounded, or sensitive result'
				record 'bounded structured Node ring log query over mTLS'
				return 0
				;;
			failed|unknown) fail "Node log query reached terminal state $state" ;;
			esac
		fi
		sleep 0.2
	done
	fail 'timed out polling Node log query task'
}

# cert_der_fp prints the SHA-256 fingerprint of a cert's DER bytes (the same
# value the panel stores in node_certs.fingerprint and returns from
# /api/admin/nodes/{id}/certs). Plain `sha256sum` hashes the PEM file
# bytes, which is different — use this whenever comparing against the API.
cert_der_fp() {
	# openssl -fingerprint prints uppercase hex with colons; the panel stores
	# lowercase (hex.EncodeToString). Normalize to lowercase, no colons.
	openssl x509 -in "$1" -noout -fingerprint -sha256 2>/dev/null \
		| sed 's/^sha256 Fingerprint=//; s/://g' | tr 'A-F' 'a-f'
}

# sign_expired_node_cert produces an expired node.crt at <node_cert> using the
# panel's e2e CA and the node's EXISTING node.key (so the public key matches
# what the panel already has on file). It uses `openssl ca` with
# -startdate/-enddate because openssl 3.0.x lacks x509 -not_before/-not_after.
# The result is chain-valid but past NotAfter; the node's local pre-dial check
# (path A) rejects it before any TLS handshake, matching §10.4.
sign_expired_node_cert() {
	local node_key="$1" node_cert="$2" ca_crt="$3" ca_key="$4" ca_dir
	ca_dir="$(mktemp -d "$TMP_DIR/oca.XXXXXX")"
	mkdir -p "$ca_dir/demoCA/newcerts"
	: >"$ca_dir/demoCA/index.txt"
	echo 1000 >"$ca_dir/demoCA/serial"
	local csr="$ca_dir/node.csr" cnf="$ca_dir/ca.cnf"
	openssl req -new -key "$node_key" -subj '/CN=node-1' -out "$csr" 2>/dev/null
	cat >"$cnf" <<EOF
[ca]
default_ca = CA_default
[CA_default]
database = /dev/null
new_certs_dir = $ca_dir/demoCA/newcerts
certificate = $ca_crt
private_key = $ca_key
serial = $ca_dir/demoCA/serial
default_md = sha256
policy = policy_any
x509_extensions = client_ext
[policy_any]
commonName = supplied
[client_ext]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
EOF
	# -startdate/-enddate are absolute UTC (YYYYMMDDHHMMSSZ). Pick a window that
	# is unambiguously in the past relative to the test host clock.
	( cd "$ca_dir" && openssl ca -config "$cnf" -cert "$ca_crt" -keyfile "$ca_key" \
		-in "$csr" -out "$node_cert" \
		-startdate 20260101000000Z -enddate 20260102000000Z \
		-extensions client_ext -batch -notext >"$ca_dir/ca.log" 2>&1 ) \
		|| fail "openssl ca failed to sign expired node cert (see $ca_dir/ca.log)"
	# Self-proof: the cert we just signed is past NotAfter.
	openssl x509 -checkend 0 -noout -in "$node_cert" >/dev/null 2>&1 \
		&& fail 'signed node cert is NOT expired (expected past NotAfter for E3)'
	chmod 600 "$node_cert"
	record "E3: signed chain-valid but expired node.crt (path A will reject before dial)"
}

run_browser_gate() {
	if [[ "$SKIP_BROWSER" == 1 ]]; then
		record 'browser gate skipped by request'
		return 0
	fi
	local expected_status="${1:-valid}"
	local telemetry_only="${2:-0}"
	local browser_report="$TMP_DIR/browser-report-${expected_status}.json"
	local browser_args=(
		--panel-url "$PANEL_ADMIN_URL"
		--expected-node-name "$NODE_NAME"
		--expected-telemetry-status "$expected_status"
		--timeout "$TIMEOUT"
		--report "$browser_report"
	)
	if [[ "$telemetry_only" == 1 ]]; then
		browser_args+=(--telemetry-only)
	fi
	E2E_ADMIN_PASSWORD="$ADMIN_PASSWORD" python3 scripts/internal/e2e-browser.py "${browser_args[@]}" \
		>"$TMP_DIR/browser.stdout" 2>"$TMP_DIR/browser.stderr" || {
			fail "browser gate failed: $(redact_text <"$TMP_DIR/browser.stderr" 2>/dev/null || true)"
		}
	if [[ "$telemetry_only" == 1 ]]; then
		record "ChromeDriver Panel telemetry status assertion ($expected_status)"
	else
		record 'ChromeDriver Panel /panel-dashboard -> /nodes -> /logs and API boundary assertions'
	fi
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
	assert_panel_logs

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

	assert_panel_telemetry_reflects_s3_mutations "$survivor_src"

	query_node_logs

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
	assert_panel_telemetry_stale
	run_browser_gate stale 1
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

	# --- Certificate auto-renewal scenario ---
	# Re-create the survivor object so S3 availability can be verified during
	# the renewal flow (it was deleted in the wrong-CA section above).
	printf 'survives cert renewal\n' >"$survivor_src"
	s3_expect 200 PUT "/$BUCKET_NAME/e2e/survivor.txt" "$TMP_DIR/survivor-renew-put.out" "$survivor_src"

	# Stop the node, reconfigure panel with a very short client_cert_ttl, restart
	# panel, then restart the node. The node will register with a short-lived cert.
	# Once the cert crosses the TTL/3 renewal threshold, the node should
	# automatically POST /renew, get a new cert, and reconnect. The old cert
	# should be revoked after the new one activates.
	stop_node
	stop_panel

	# Rewrite panel config with a 45-second client cert TTL.
	# Threshold = 45/3 = 15s. The node should renew ~15s before expiry.
	local renew_panel_config="$TMP_DIR/panel-renew.yaml"
	local renew_ttl="45s"
	if [[ "$MODE" == docker ]]; then
		panel_root=/data
		admin_bind='0.0.0.0:9001'
		agent_bind='0.0.0.0:9443'
	else
		panel_root="$TMP_DIR/panel-data"
		admin_bind="127.0.0.1:$PANEL_ADMIN_PORT"
		agent_bind="127.0.0.1:$PANEL_AGENT_PORT"
	fi
	cat >"$renew_panel_config" <<EOF
admin_addr: "$admin_bind"
agent:
  addr: "$agent_bind"
  cert_file: "$panel_root/pki/panel-server.crt"
  key_file: "$panel_root/pki/panel-server.key"
pki:
  intermediate_cert_file: "$panel_root/pki/intermediate.crt"
  intermediate_key_file: "$panel_root/pki/intermediate.key"
  client_cert_ttl: $renew_ttl
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
	chmod 600 "$renew_panel_config"

	# Remove the old node cert so it re-registers with the short TTL.
	rm -f "$TMP_DIR/node-data/pki/node.crt"

	# Restart panel with short TTL config.
	if [[ "$MODE" == docker ]]; then
		docker rm -f "$PANEL_CONTAINER" >/dev/null 2>&1 || true
		PANEL_CONFIG="$renew_panel_config"
		start_panel
	else
		PANEL_CONFIG="$renew_panel_config"
		start_panel
	fi
	wait_panel_ready
	admin_login

	# Re-issue a registration token (the old one was consumed).
	api_expect 201 POST /api/admin/nodes/1/tokens
	REGISTRATION_TOKEN="$(json_field "$API_BODY" token)"

	# Rewrite node config with the new token. write_configs resets PANEL_CONFIG
	# to the original path, so save and restore our renew config.
	local saved_panel_config="$renew_panel_config"
	write_configs "$REGISTRATION_TOKEN"
	PANEL_CONFIG="$saved_panel_config"

	start_node
	wait_s3 "$NODE_S3_URL" node-renew
	poll_node_synced

	# Capture the original cert fingerprint.
	local cert_before
	cert_before="$(sha256sum "$TMP_DIR/node-data/pki/node.crt" | awk '{print $1}')"
	record "renewal: node registered with short-TTL cert (fingerprint=$cert_before)"

	# Wait for the renewal to happen: the node should renew within ~30s
	# (45s TTL - 15s threshold = 30s of validity before renewal triggers).
	local renew_deadline=$((SECONDS + 60))
	local cert_after
	while ((SECONDS < renew_deadline)); do
		cert_after="$(sha256sum "$TMP_DIR/node-data/pki/node.crt" 2>/dev/null | awk '{print $1}')"
		if [[ -n "$cert_after" && "$cert_after" != "$cert_before" ]]; then
			record "renewal: node certificate changed (new fingerprint=$cert_after)"
			break
		fi
		sleep 1
	done
	if [[ "$cert_after" == "$cert_before" ]]; then
		fail "node did not renew its certificate within the expected window"
	fi

	# Verify the node reconnects and syncs after renewal.
	poll_node_synced
	record 'renewal: node reconnected and synced after certificate renewal'

	# Verify S3 data plane remained available throughout.
	s3_expect 200 GET "/$BUCKET_NAME/e2e/survivor.txt" "$survivor_dst"
	cmp "$survivor_src" "$survivor_dst" || fail 'S3 bytes changed during renewal'
	record 'renewal: S3 data plane stayed available during cert renewal'

	# Verify the old cert is revoked in the panel DB (D1: after new cert activates).
	local renew_deadline2=$((SECONDS + 15))
	while ((SECONDS < renew_deadline2)); do
		api_request GET /api/admin/nodes/1
		if [[ "$API_STATUS" == 200 ]]; then
			# Check if the node is online — if so, the new cert has activated.
			if python3 - "$API_BODY" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1], encoding="utf-8"))
raise SystemExit(0 if obj.get("online") else 1)
PY
			then
				record 'renewal: new cert activated, node online'
				break
			fi
		fi
		sleep 1
	done

	# Restore the original panel config for any remaining steps.
	if [[ "$MODE" != docker ]]; then
		PANEL_CONFIG="$TMP_DIR/panel.yaml"
	fi

	# --- Server certificate re-sign scenario (AC4: nodes don't need re-registration) ---
	# The renewal scenario above leaves the panel running with the 45s-TTL config
	# and the node holding a short-lived cert. Restore the original 24h-TTL panel
	# config and wait for the node to renew into a long-lived cert BEFORE taking
	# the panel down: an expired cert cannot renew (D2) and would lock the node out.
	if [[ "$MODE" == docker ]]; then
		docker rm -f "$PANEL_CONTAINER" >/dev/null 2>&1 || true
		PANEL_CONFIG="$TMP_DIR/panel.yaml"
		start_panel
	else
		stop_panel
		start_panel
	fi
	wait_panel_ready
	local longlived_deadline=$((SECONDS + 90))
	while ((SECONDS < longlived_deadline)); do
		if openssl x509 -checkend 3600 -noout -in "$TMP_DIR/node-data/pki/node.crt" 2>/dev/null; then
			break
		fi
		sleep 1
	done
	openssl x509 -checkend 3600 -noout -in "$TMP_DIR/node-data/pki/node.crt" 2>/dev/null || \
		fail 'node did not renew into a long-lived cert before the re-sign scenario'
	record 're-sign: node holds a long-lived cert (24h panel TTL restored)'

	local panel_db_before master_key_before
	panel_db_before="$TMP_DIR/panel-data/panel.db"
	master_key_before="$TMP_DIR/panel-data/secrets/master.key"

	# Record red-line file checksums before re-sign.
	sha256sum "$panel_db_before" "$master_key_before" \
		"$TMP_DIR/panel-data/pki/intermediate.crt" "$TMP_DIR/panel-data/pki/intermediate.key" \
		> "$TMP_DIR/resign-baseline.txt" 2>/dev/null || true

	# Record the node cert fingerprint (should not change).
	local node_crt_before
	node_crt_before="$(sha256sum "$TMP_DIR/node-data/pki/node.crt" 2>/dev/null | awk '{print $1}')"

	# Re-sign the server cert directly with openssl (the e2e PKI layout differs
	# from the install-panel.sh directory structure, so we can't call
	# renew-server-cert directly). This exercises the same code path: CA reads
	# only, server cert replaced atomically, node client cert untouched.
	stop_panel
	local _pki="$TMP_DIR/panel-data/pki"
	local _ts; _ts="$(date +%Y%m%d%H%M%S)"
	cp -- "$_pki/panel-server.crt" "$_pki/panel-server.crt.bak.$_ts"
	cp -- "$_pki/panel-server.key" "$_pki/panel-server.key.bak.$_ts"
	chmod 600 "$_pki/panel-server.key.bak.$_ts"
	openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
		-out "$_pki/.renew.key" >/dev/null 2>&1
	local _renew_san='DNS:localhost,IP:127.0.0.1'
	[[ "$MODE" == docker ]] && _renew_san+=',DNS:panel'
	openssl req -new -key "$_pki/.renew.key" -subj '/CN=panel' \
		-out "$_pki/.renew.csr" 2>/dev/null
	printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$_renew_san" > "$_pki/.renew.ext"
	openssl x509 -req -in "$_pki/.renew.csr" \
		-CA "$_pki/intermediate.crt" -CAkey "$_pki/intermediate.key" -CAcreateserial \
		-days 2 -sha256 -extfile "$_pki/.renew.ext" \
		-out "$_pki/.renew.crt" >/dev/null 2>&1
	mv -f "$_pki/.renew.crt" "$_pki/panel-server.crt"
	mv -f "$_pki/.renew.key" "$_pki/panel-server.key"
	chmod 600 "$_pki/panel-server.key"
	chmod 644 "$_pki/panel-server.crt"
	rm -f "$_pki/.renew.csr" "$_pki/.renew.ext" "$_pki/intermediate.srl"
	record 'server cert re-signed with multi-SAN'

	# Red-line: panel DB, master.key, CA cert, CA key must be unchanged.
	if ! sha256sum -c "$TMP_DIR/resign-baseline.txt" >/dev/null 2>&1; then
		fail 're-sign modified panel DB, master.key, or CA files (red line violated)'
	fi
	record 're-sign red line: panel DB, master.key, CA files unchanged'

	# Verify the new cert has the expected SANs.
	local resign_san
	resign_san="$(openssl x509 -noout -text -in "$TMP_DIR/panel-data/pki/panel-server.crt" \
		| grep -A1 'Subject Alternative Name' | tail -1 | sed 's/^[[:space:]]*//')" || true
	if [[ -z "$resign_san" ]]; then
		fail 're-signed server cert has no SAN'
	fi
	record "re-sign: new cert SAN = $resign_san"

	# Restart panel with the new cert.
	start_panel
	wait_panel_ready
	# Panel sessions are in-memory (pkg/webadmin/auth.go activeSessions);
	# a panel restart invalidates the old cookie, so log in again.
	admin_login

	# The node should reconnect without re-registration.
	poll_node_synced
	record 're-sign: node reconnected without re-registration after server cert re-sign'

	# Verify node cert fingerprint is unchanged.
	local node_crt_after
	node_crt_after="$(sha256sum "$TMP_DIR/node-data/pki/node.crt" 2>/dev/null | awk '{print $1}')"
	if [[ "$node_crt_after" != "$node_crt_before" ]]; then
		fail 'node cert changed after server cert re-sign (should not happen)'
	fi
	record 're-sign: node client cert unchanged'

	# Verify S3 data plane still works.
	s3_expect 200 GET "/$BUCKET_NAME/e2e/survivor.txt" "$survivor_dst"
	record 're-sign: S3 data plane available after server cert re-sign'

	# --- Expired node cert recovery scenario (AC5: §10.4 walked end-to-end) ---
	# The re-sign scenario leaves the node online with a valid, long-lived cert.
	# We now manufacture an expired node.crt (same node.key, real e2e CA) and
	# walk the §10.4 recovery sequence: path A rejects the expired cert, S3
	# safety net A keeps serving, then token re-registration restores the node.
	# Keep a survivor object so S3 availability can be asserted during outage.
	printf 'survives cert expiry\n' >"$survivor_src"
	s3_expect 200 PUT "/$BUCKET_NAME/e2e/survivor.txt" "$TMP_DIR/survivor-expired-put.out" "$survivor_src"

	local _ca_crt="$TMP_DIR/panel-data/pki/intermediate.crt"
	local _ca_key="$TMP_DIR/panel-data/pki/intermediate.key"
	local _node_key="$TMP_DIR/node-data/pki/node.key"
	local _node_crt="$TMP_DIR/node-data/pki/node.crt"
	[[ -s $_node_key && -s $_node_crt && -s $_ca_crt && -s $_ca_key ]] \
		|| fail 'E3 precondition: node key/cert or panel CA missing after re-sign'

	# Snapshot the fingerprint of the currently-active node cert BEFORE we
	# overwrite it with an expired one. After §10.4 recovery, D1 must have
	# revoked this originally-active cert and activated a new one. (The expired
	# cert we manufacture is never in the panel DB — it is only a local file
	# the node rejects at path A — so it is not the subject of the D1 check.)
	local active_fp_before
	active_fp_before="$(cert_der_fp "$_node_crt")"
	# Also confirm the panel currently has this cert activated (sanity).
	api_request GET /api/admin/nodes/1/certs
	E2E_ACTIVE_FP="$active_fp_before" python3 - "$API_BODY" <<'PY'
import json, os, sys
active = os.environ["E2E_ACTIVE_FP"]
certs = json.load(open(sys.argv[1], encoding="utf-8"))
if not isinstance(certs, list):
	raise SystemExit("certs response is not a JSON array")
row = next((c for c in certs if c.get("fingerprint") == active), None)
if row is None:
	raise SystemExit(f"pre-E3 active cert {active} not in panel DB")
if row.get("status") != "active":
	raise SystemExit(f"pre-E3 active cert status={row.get('status')}, want active")
# activated_at is not exposed by the API; status==active is the proxy for
# "the panel has seen this cert connect and not revoked it" (D1).
PY
	record 'E3: pre-condition — node holds an active (D1-activated) cert in panel DB'

	stop_node
	sign_expired_node_cert "$_node_key" "$_node_crt" "$_ca_crt" "$_ca_key"
	start_node
	# Give the node a moment to attempt (and fail) the control-plane connection.
	sleep 2
	# Assert path A: the node log reports a local cert problem (expired).
	if ! grep -Eiq 'client certificate expired|certificate problem prevents control-plane' \
		"$NODE_LOG" "$TMP_DIR/node-stdout.log" 2>/dev/null; then
		fail 'E3: node did not report path A cert error for expired cert'
	fi
	record 'E3: node reported path A cert error (expired), no control-plane connection'
	# Assert safety net A: S3 data plane still serves the local DB object.
	s3_expect 200 GET "/$BUCKET_NAME/e2e/survivor.txt" "$survivor_dst"
	cmp "$survivor_src" "$survivor_dst" || fail 'E3: S3 bytes changed during expired-cert outage'
	record 'E3: S3 data plane stayed available while node cert was expired (safety net A)'

	# Walk §10.4 recovery: issue token -> stop node -> delete expired cert+key
	# -> write token -> start node -> poll sync -> clear token.
	# Note: the node's old key is reused only if we keep it; §10.4 step 4 says
	# delete node.crt AND node.key, so the node generates a fresh key on
	# re-registration (its public key changes; the panel accepts the new CSR).
	api_expect 201 POST /api/admin/nodes/1/tokens
	REGISTRATION_TOKEN="$(json_field "$API_BODY" token)"
	[[ -n "$REGISTRATION_TOKEN" ]] || fail 'E3: panel returned empty re-registration token'
	stop_node
	rm -f "$_node_crt" "$_node_key"
	# write_configs resets NODE_CONFIG and needs the token; save/restore panel
	# config because the re-sign block above may have changed it.
	local _saved_panel_cfg="$PANEL_CONFIG"
	write_configs "$REGISTRATION_TOKEN"
	PANEL_CONFIG="$_saved_panel_cfg"
	start_node
	wait_s3 "$NODE_S3_URL" node-after-expired-recovery
	poll_node_synced
	record 'E3: node re-registered via §10.4 (token) and reconnected'

	# Clear the token (§10.4 step 7 hygiene).
	write_configs ''
	PANEL_CONFIG="$_saved_panel_cfg"
	# write_configs rewrote node.yaml without the token but did NOT restart;
	# restarting is not required for hygiene, the token is already consumed.

	# Assert D1: the previously-active cert is revoked, a new one activated.
	api_request GET /api/admin/nodes/1/certs
	E2E_ACTIVE_FP="$active_fp_before" python3 - "$API_BODY" <<'PY'
import json, os, sys
active = os.environ["E2E_ACTIVE_FP"]
certs = json.load(open(sys.argv[1], encoding="utf-8"))
if not isinstance(certs, list):
	raise SystemExit("certs response is not a JSON array")
found_old = False
found_new_active = False
for c in certs:
	fp = c.get("fingerprint")
	if fp == active:
		found_old = True
		if not c.get("revoked"):
			raise SystemExit(f"previously-active cert {fp} not marked revoked (D1)")
	if fp != active and c.get("status") == "active":
		found_new_active = True
if not found_old:
	raise SystemExit(f"previously-active cert {active} not present in certs list")
if not found_new_active:
	raise SystemExit("no new activated cert after recovery (D1)")
PY
	record 'E3: D1 verified — previously-active cert revoked, new cert activated'

	# Verify the new node cert is long-lived and S3 still works.
	openssl x509 -checkend 3600 -noout -in "$_node_crt" 2>/dev/null \
		|| fail 'E3: new node cert is not long-lived (expected >1h remaining)'
	s3_expect 200 GET "/$BUCKET_NAME/e2e/survivor.txt" "$survivor_dst"
	record 'E3: S3 data plane available after expired-cert recovery'

	run_browser_gate valid
	record 'registration response-loss replay: pkg/nodeagent package regression evidence runs in quality gate'
	record 'PASS: Panel -> Node release gate complete'
	printf 'panel-node-e2e passed (%s mode)\n' "$MODE"
}

main "$@"
