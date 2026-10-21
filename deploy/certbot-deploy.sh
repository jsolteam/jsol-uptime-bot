#!/bin/sh
set -eu

if [ -z "${RENEWED_LINEAGE:-}" ]; then
	echo "RENEWED_LINEAGE не задан Certbot" >&2
	exit 1
fi

script_path=$(readlink -f "$0")
project_dir=$(CDPATH= cd -- "$(dirname -- "$script_path")/.." && pwd)
cert_dir="$project_dir/certs"

install -d "$cert_dir"
install -o 10001 -g 10001 -m 0444 "$RENEWED_LINEAGE/fullchain.pem" "$cert_dir/fullchain.pem"
install -o 10001 -g 10001 -m 0400 "$RENEWED_LINEAGE/privkey.pem" "$cert_dir/privkey.pem"
docker compose -f "$project_dir/compose.yaml" restart uptime-bot
