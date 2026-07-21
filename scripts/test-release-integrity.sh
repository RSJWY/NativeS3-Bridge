#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$root_dir/scripts/lib/integration-test-helpers.sh"
cd "$root_dir"

for command_name in go openssl curl python3; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		printf 'release integrity test requires %s\n' "$command_name" >&2
		exit 1
	fi
done

tmp_dir="$(mktemp -d /tmp/natives3-release-integrity.XXXXXX)"
panel_pid=""

cleanup() {
	stop_process "$panel_pid"
	rm -rf "$tmp_dir"
}
trap cleanup EXIT

printf 'building current panel and node\n'
GOWORK=off go build -o "$tmp_dir/panel" ./cmd/panel
GOWORK=off go build -o "$tmp_dir/node" ./cmd/node

mkdir -p "$tmp_dir/pki" "$tmp_dir/secrets"
head -c 32 /dev/urandom > "$tmp_dir/secrets/master.key"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$tmp_dir/pki/intermediate.key" >/dev/null 2>&1
openssl req -x509 -new -key "$tmp_dir/pki/intermediate.key" -sha256 -days 2 \
	-subj '/CN=NativeS3 Test Intermediate CA' \
	-addext 'basicConstraints=critical,CA:TRUE' \
	-addext 'keyUsage=critical,keyCertSign,cRLSign' \
	-out "$tmp_dir/pki/intermediate.crt" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$tmp_dir/pki/panel-server.key" >/dev/null 2>&1
openssl req -new -key "$tmp_dir/pki/panel-server.key" -subj '/CN=localhost' \
	-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
	-out "$tmp_dir/pki/panel-server.csr" >/dev/null 2>&1
printf 'basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n' > "$tmp_dir/pki/server.ext"
openssl x509 -req -in "$tmp_dir/pki/panel-server.csr" \
	-CA "$tmp_dir/pki/intermediate.crt" -CAkey "$tmp_dir/pki/intermediate.key" -CAcreateserial \
	-days 2 -sha256 -extfile "$tmp_dir/pki/server.ext" \
	-out "$tmp_dir/pki/panel-server.crt" >/dev/null 2>&1

admin_port="$(pick_free_port)"
agent_port="$(pick_free_port)"
cat > "$tmp_dir/panel.yaml" <<EOF
admin_addr: "127.0.0.1:$admin_port"
agent:
  addr: "127.0.0.1:$agent_port"
  cert_file: "$tmp_dir/pki/panel-server.crt"
  key_file: "$tmp_dir/pki/panel-server.key"
pki:
  intermediate_cert_file: "$tmp_dir/pki/intermediate.crt"
  intermediate_key_file: "$tmp_dir/pki/intermediate.key"
  client_cert_ttl: 24h
master_key_file: "$tmp_dir/secrets/master.key"
database:
  driver: sqlite
  dsn: "$tmp_dir/panel.db"
webadmin:
  admin_bootstrap_password: "release-integrity-password"
  session_secret: "release-integrity-session-secret-32b"
log_level: info
EOF

cat > "$tmp_dir/node.yaml" <<EOF
server:
  s3_addr: "127.0.0.1:$(pick_free_port)"
  admin_addr: "legacy-admin-field-is-ignored"
storage:
  data_root: "$tmp_dir/node-data"
database:
  driver: sqlite
  dsn: "$tmp_dir/node.db"
panel:
  node_id: 1
  agent_url: "wss://127.0.0.1:$agent_port/agent"
  cert_file: "$tmp_dir/node-pki/node.crt"
  key_file: "$tmp_dir/node-pki/node.key"
  ca_file: "$tmp_dir/pki/intermediate.crt"
webadmin:
  session_secret: "legacy-field-is-ignored"
rate_limit:
  anonymous_rps: 99
region: us-east-1
EOF

"$tmp_dir/panel" -check-config -config "$tmp_dir/panel.yaml" >/dev/null
"$tmp_dir/node" -check-config -config "$tmp_dir/node.yaml" >/dev/null

mv "$tmp_dir/secrets/master.key" "$tmp_dir/secrets/master.key.missing"
if "$tmp_dir/panel" -check-config -config "$tmp_dir/panel.yaml" >"$tmp_dir/missing-master.log" 2>&1; then
	printf 'panel check-config unexpectedly passed without the master key\n' >&2
	exit 1
fi
mv "$tmp_dir/secrets/master.key.missing" "$tmp_dir/secrets/master.key"

mv "$tmp_dir/pki/intermediate.crt" "$tmp_dir/pki/intermediate.crt.missing"
if "$tmp_dir/panel" -check-config -config "$tmp_dir/panel.yaml" >"$tmp_dir/missing-ca.log" 2>&1; then
	printf 'panel check-config unexpectedly passed without the intermediate CA\n' >&2
	exit 1
fi
mv "$tmp_dir/pki/intermediate.crt.missing" "$tmp_dir/pki/intermediate.crt"

"$tmp_dir/panel" -config "$tmp_dir/panel.yaml" >"$tmp_dir/panel.log" 2>&1 &
panel_pid=$!
wait_http "http://127.0.0.1:$admin_port/healthz" "$panel_pid" "$tmp_dir/panel.log"
stop_process "$panel_pid"
panel_pid=""

bash ./scripts/test-distribution-contract.sh
bash ./scripts/test-upgrade-rollback.sh

printf 'release integrity integration tests passed\n'
