#!/usr/bin/env bash

pick_free_port() {
	python3 - <<'PY'
import socket

sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
}

wait_http() {
	url="$1"
	pid="$2"
	log_file="$3"
	for _ in $(seq 1 100); do
		if curl --silent --max-time 1 --output /dev/null "$url" 2>/dev/null; then
			return 0
		fi
		if ! kill -0 "$pid" >/dev/null 2>&1; then
			printf 'process %s exited before %s became ready\n' "$pid" "$url" >&2
			cat "$log_file" >&2 || true
			return 1
		fi
		sleep 0.1
	done
	printf 'timed out waiting for %s\n' "$url" >&2
	cat "$log_file" >&2 || true
	return 1
}

stop_process() {
	pid="$1"
	if [ -z "$pid" ] || ! kill -0 "$pid" >/dev/null 2>&1; then
		return 0
	fi
	kill -TERM "$pid" >/dev/null 2>&1 || true
	for _ in $(seq 1 50); do
		if ! kill -0 "$pid" >/dev/null 2>&1; then
			wait "$pid" >/dev/null 2>&1 || true
			return 0
		fi
		sleep 0.1
	done
	kill -KILL "$pid" >/dev/null 2>&1 || true
	wait "$pid" >/dev/null 2>&1 || true
}
