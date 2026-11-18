#!/usr/bin/env bash
# A throwaway Synapse in Docker for the Matrix backend's end-to-end tests
# (internal/matrix/**, build tag matrixe2e). Never point the tests at a real
# account or homeserver.
#
#   scripts/matrix-dev/synapse.sh start   start it (or reuse a running one)
#   scripts/matrix-dev/synapse.sh env     print `export` lines for the tests
#   scripts/matrix-dev/synapse.sh url     print the client-server API URL
#   scripts/matrix-dev/synapse.sh stop    remove the container and its data
#
# The server is named "localhost", has open registration off, a random
# registration_shared_secret (for /_synapse/admin/v1/register) and rate
# limits raised out of the way. Its config and database live inside the
# container (an anonymous volume on /data), so `stop` deletes everything
# and nothing is written to the host. Docker picks a free host port on
# 127.0.0.1.
#
# Environment:
#   MATRIX_DEV_CONTAINER  container name (default slk-matrix-dev)
#   MATRIX_DEV_IMAGE      image (default matrixdotorg/synapse:v1.165.0)
#   MATRIX_DEV_TIMEOUT    seconds to wait for the server (default 120)
set -euo pipefail

name=${MATRIX_DEV_CONTAINER:-slk-matrix-dev}
image=${MATRIX_DEV_IMAGE:-matrixdotorg/synapse:v1.165.0}
timeout=${MATRIX_DEV_TIMEOUT:-120}

die() {
	echo "synapse.sh: $*" >&2
	exit 1
}

running() {
	[ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = true ]
}

url() {
	local hostport
	hostport=$(docker port "$name" 8008/tcp 2>/dev/null | grep -m1 '^127\.0\.0\.1:') ||
		die "container $name has no published port; is it running?"
	echo "http://$hostport"
}

secret() {
	docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$name" |
		sed -n 's/^SLK_SHARED_SECRET=//p'
}

wait_ready() {
	local u deadline
	u=$(url)
	deadline=$((SECONDS + timeout))
	until curl -fsS "$u/_matrix/client/versions" >/dev/null 2>&1; do
		if ! running; then
			docker logs --tail 50 "$name" >&2 || true
			die "container $name exited while starting"
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			docker logs --tail 50 "$name" >&2 || true
			die "Synapse at $u not ready after ${timeout}s"
		fi
		sleep 1
	done
}

rate_limit() {
	printf '%s:\n  per_second: 100000\n  burst_count: 1000000\n' "$1"
}

config() {
	local secret=$1 macaroon=$2 form=$3
	cat <<EOF
server_name: "localhost"
pid_file: /data/homeserver.pid
listeners:
  - port: 8008
    tls: false
    type: http
    x_forwarded: true
    resources:
      - names: [client]
        compress: false
database:
  name: sqlite3
  args:
    database: /data/homeserver.db
log_config: "/data/localhost.log.config"
media_store_path: /data/media_store
signing_key_path: "/data/localhost.signing.key"
registration_shared_secret: "$secret"
macaroon_secret_key: "$macaroon"
form_secret: "$form"
enable_registration: false
report_stats: false
trusted_key_servers: []
suppress_key_server_warning: true
federation_domain_whitelist: []
presence:
  enabled: true
EOF
	for rc in rc_message rc_registration rc_admin_redaction rc_joins_per_room \
		rc_3pid_validation rc_third_party_invite rc_media_create \
		rc_key_requests rc_room_creation rc_reports rc_delayed_event_mgmt; do
		rate_limit "$rc"
	done
	printf 'rc_login:\n'
	for k in address account failed_attempts; do rate_limit "  $k" | sed '2,$s/^/  /'; done
	printf 'rc_joins:\n'
	for k in local remote; do rate_limit "  $k" | sed '2,$s/^/  /'; done
	printf 'rc_invites:\n'
	for k in per_room per_user per_issuer; do rate_limit "  $k" | sed '2,$s/^/  /'; done
	printf 'rc_presence:\n'
	rate_limit "  per_user" | sed '2,$s/^/  /'
}

rand() {
	od -An -N24 -tx1 /dev/urandom | tr -d ' \n'
}

start() {
	command -v docker >/dev/null || die "docker not found"
	if running; then
		wait_ready
		url
		return
	fi
	# A stopped or half-created leftover: start from scratch.
	docker rm -f -v "$name" >/dev/null 2>&1 || true

	local secret
	secret=$(rand)
	docker run -d --name "$name" \
		--label slk-matrix-dev=1 \
		-p 127.0.0.1::8008 \
		-e SYNAPSE_SERVER_NAME=localhost \
		-e SYNAPSE_REPORT_STATS=no \
		-e SLK_SHARED_SECRET="$secret" \
		-e SLK_HOMESERVER_YAML="$(config "$secret" "$(rand)" "$(rand)")" \
		--entrypoint sh \
		"$image" -c '
			set -e
			mkdir -p /data
			chown 991:991 /data
			/start.py generate >/dev/null
			printf "%s\n" "$SLK_HOMESERVER_YAML" >/data/homeserver.yaml
			exec /start.py
		' >/dev/null
	wait_ready
	url
}

stop() {
	docker rm -f -v "$name" >/dev/null 2>&1 || true
}

case "${1:-}" in
start) start ;;
stop) stop ;;
url) url ;;
env)
	running || die "container $name is not running; run: $0 start"
	echo "export MATRIX_E2E_HOMESERVER=$(url)"
	echo "export MATRIX_E2E_SHARED_SECRET=$(secret)"
	;;
*)
	echo "usage: $0 start|stop|env|url" >&2
	exit 2
	;;
esac
