#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

require_text() {
	file="$1"
	text="$2"
	if ! grep -Fq "$text" "$file"; then
		printf 'distribution contract failed: %s is missing %s\n' "$file" "$text" >&2
		exit 1
	fi
}

require_text Dockerfile 'FROM alpine:3.20 AS panel'
require_text Dockerfile 'FROM alpine:3.20 AS node'
require_text Dockerfile 'FROM go-base AS panel-build'
require_text Dockerfile 'FROM go-base AS node-build'
require_text Dockerfile 'EXPOSE 9001 9443'
require_text Dockerfile 'EXPOSE 9000'
require_text Dockerfile 'ENTRYPOINT ["panel"]'
require_text Dockerfile 'ENTRYPOINT ["node"]'
require_text docker-compose.example.yml 'target: panel'
require_text docker-compose.example.yml 'target: node'
require_text docker-compose.example.yml 'test: ["CMD", "/usr/local/bin/panel", "-check-config"'
require_text docker-compose.example.yml 'test: ["CMD", "/usr/local/bin/node", "-check-config"'

node_build_stage="$(awk '/^FROM go-base AS node-build$/{in_stage=1} in_stage{print} in_stage && /^FROM / && $0 !~ /AS node-build$/{exit}' Dockerfile)"
if grep -Eq -- '--from=web|cmd/panel' <<<"$node_build_stage"; then
	printf 'distribution contract failed: node-build depends on the web or panel build\n' >&2
	exit 1
fi

if awk '/^  node:/{in_node=1; next} in_node && /^  [a-zA-Z0-9_-]+:/{in_node=0} in_node && /9001|9443/' docker-compose.example.yml | grep -q .; then
	printf 'distribution contract failed: node service publishes a management port\n' >&2
	exit 1
fi

printf 'distribution contract passed\n'
