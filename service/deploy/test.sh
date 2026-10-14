#!/bin/sh
# SPDX-FileCopyrightText: 2026 Reza Jelveh
# SPDX-License-Identifier: Apache-2.0
# Upstream publishes a signed release to the registry; the operator imports it.
# Usage: test.sh | test.sh down
set -eu
cd "$(dirname "$0")"
COSIGN=gcr.io/projectsigstore/cosign:v2.6.1@sha256:68839b7f13dac5a6744a5d8818e984dd39183374e37855c19e14d623d9bc9037
UP=18080 OP=18180

if [ "${1:-}" = down ]; then exec docker compose down -v; fi

die() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== $*"; }
# api <port> <method> <path> [body]: sets R to the response body; dies on non-2xx.
api() {
	key=$UK; [ "$1" = $OP ] && key=$OK
	R=$(curl -sS --fail-with-body -X "$2" -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
		${4:+-d "$4"} "http://127.0.0.1:$1$3") || die "$2 $3 on :$1: $R"
}
j() { echo "$R" | jq -r "$1"; }
# await <port> <path>: polls a run until it succeeded (R holds it) or failed.
await() {
	i=0
	while [ $i -lt 300 ]; do
		api "$1" GET "$2"
		case $(j .status) in
		succeeded) return ;;
		failed) die "$2 failed: $(j .error)" ;;
		esac
		i=$((i + 1)); sleep 2
	done
	die "$2 not finished after 600s"
}
mint() { docker compose exec -T "$1" /catalog-service auth issue -name test | sed -n 's/^key: //p'; }

mkdir -p keys
if [ ! -f keys/cosign.key ]; then
	step "generating cosign key pair"
	od -An -N16 -tx1 /dev/urandom | tr -d ' \n' >keys/password
	docker run --rm -u "$(id -u):$(id -g)" -e COSIGN_PASSWORD="$(cat keys/password)" -v "$PWD/keys:/keys" -w /keys \
		"$COSIGN" generate-key-pair >/dev/null
	chmod 644 keys/cosign.key # read by the service's nonroot uid
fi
COSIGN_PASSWORD=$(cat keys/password)
export COSIGN_PASSWORD

step "up"
docker compose --progress quiet up -d --build --quiet-pull
for port in $UP $OP; do
	i=0
	until curl -sf "http://127.0.0.1:$port/readyz" >/dev/null; do
		i=$((i + 1)); [ $i -le 60 ] || die ":$port/readyz not ready after 120s"
		sleep 2
	done
done
UK=$(mint upstream); OK=$(mint operator)
[ -n "$UK" ] && [ -n "$OK" ] || die "minting API keys"

step "upstream: import bootstrap-from-json"
api $UP POST /v1/imports '{"importer":"bootstrap-from-json"}'
await $UP "/v1/imports/$(j .id)"
j '"\(.status) \(.input_version)"'

step "upstream: generator run"
api $UP POST /v1/generator-runs
gen=$(j .id)
await $UP "/v1/generator-runs/$gen"
j '"\(.status) \(.input_digest)"'

step "upstream: release + publish"
api $UP POST /v1/releases "{\"generator_run\":\"$gen\"}"
ver=$(j .version) digest=$(j .digest)
api $UP POST "/v1/releases/$ver/publish"
[ "$(j '.artifact_ref // ""')" ] || die "publish returned no artifact_ref"
echo "release $ver $digest -> $(j .artifact_ref)"

step "operator: import upstream"
api $OP POST /v1/imports '{"importer":"upstream"}'
await $OP "/v1/imports/$(j .id)"
[ "$(j .input_version)" = "$ver $digest" ] || die "operator imported '$(j .input_version)', want '$ver $digest'"
j '"\(.status) \(.input_version)"'

step "operator: catalog"
api $UP GET /v1/catalog
want=$(j '.models | length')
api $OP GET /v1/catalog
n=$(j '.models | length')
[ "$n" -gt 0 ] && [ "$n" = "$want" ] || die "operator catalog has $n models, upstream $want"
echo "operator catalog: $n models, release $ver"
echo PASS
