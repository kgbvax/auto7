#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs the auto7 integration tests. The script starts a Mosquitto broker in
# a container on a free loopback port, waits until the broker answers, runs
# the Go tests with the build tag "integration", and stops the container on
# exit. Pass extra arguments of "go test" after the script name.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
image=eclipse-mosquitto:2

cid=$(docker run -d --rm -p 127.0.0.1::1883 \
	-v "$here/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro" \
	"$image")
trap 'docker stop "$cid" >/dev/null 2>&1 || true' EXIT INT TERM

port=$(docker port "$cid" 1883/tcp | sed -n 's/^127\.0\.0\.1:\([0-9]*\)$/\1/p' | head -n 1)
if [ -z "$port" ]; then
	echo "run.sh: no port for the broker" >&2
	exit 1
fi

ready=0
i=0
while [ "$i" -lt 50 ]; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null &&
		docker exec "$cid" mosquitto_pub -h 127.0.0.1 -p 1883 -t auto7/it/ready -m ready >/dev/null 2>&1; then
		ready=1
		break
	fi
	i=$((i + 1))
	sleep 0.2
done
if [ "$ready" -ne 1 ]; then
	echo "run.sh: the broker on port $port does not answer" >&2
	docker logs "$cid" >&2 || true
	exit 1
fi

export AUTO7_IT_BROKER="tcp://127.0.0.1:$port"
cd "$root"
go test -race -count=1 -tags integration "$@" ./test/integration/...
