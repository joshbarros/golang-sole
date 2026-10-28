#!/usr/bin/env bash
# What Kubernetes buys: kill an API replica and the RabbitMQ pod in the middle of a raffle and check that nothing is lost or
# done twice. Entries are accepted, the draw still happens exactly once, one winner is sold, and every message the broker
# held comes back from its persistent volume. Needs the cluster from `make cluster-up`.
set -euo pipefail

API="${API:-http://127.0.0.1:5228}"
CTX="${CTX:-kind-golang-sole}"
K="kubectl --context $CTX -n sole"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'
say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }
# every call retries a few times: for a moment after a pod dies the Service may still send a request to it
retry() { for _ in $(seq 1 20); do if out="$("$@" 2>/dev/null)"; then printf '%s' "$out"; return 0; fi; sleep 1; done; return 1; }

login() { retry curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"admin@sole.local\",\"password\":\"${ADMIN_PASSWORD:-SoleAdmin2026}\"}" | token_of; }
register() { retry curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"Resil-Passw0rd1\"}" | token_of; }
ADM="Authorization: Bearer $(login)"
A="Authorization: Bearer $(register a)"
B="Authorization: Bearer $(register b)"
C="Authorization: Bearer $(register c)"

echo "1. a raffle for one pair that draws in 100 seconds, with three entries in"
AT="$(python3 -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=100)).strftime('%Y-%m-%dT%H:%M:%S.000Z'))")"
retry curl -sf -X POST "$API/api/raffles" -H "$JSON" -H "$ADM" -d "{\"code\":\"RES-$RUN\",\"name\":\"Resilience\",\"colorway\":\"Volt\",\"brand\":\"Nimbus\",\"pairsTotal\":1,\"priceCents\":5000,\"drawAt\":\"$AT\"}" >/dev/null || fail "could not open the raffle"
H0="$(retry curl -sf -H "$ADM" "$API/api/raffles/RES-$RUN" | jget "['seedHash']")"
enter() { retry curl -sf -X POST "$API/api/orders" -H "$JSON" -H "$1" -d "{\"raffleCode\":\"RES-$RUN\",\"reference\":\"ENT-$RUN-$2\",\"pairs\":1}" | jget "['id']"; }
IDA="$(enter "$A" a)"; IDB="$(enter "$B" b)"

echo "2. kill an API replica and the RabbitMQ pod while the entries are still in flight"
$K delete "$($K get pods -l app.kubernetes.io/name=api -o name | head -1)" --wait=false | sed 's/^/  /' 
$K delete pod rabbitmq-0 --wait=false | sed 's/^/  /'
IDC="$(enter "$C" c)" # accepted even now: the order and its outbox row are one local transaction
say "a third entry was accepted while the broker was down"

echo "3. everything comes back: the draw happens once, one pair is sold, the other two are told they were not drawn"
$K rollout status statefulset/rabbitmq --timeout=180s >/dev/null
$K rollout status deployment/api --timeout=180s >/dev/null
status() { body="$(retry curl -sf -H "$1" "$API/api/orders/$2")" && printf '%s' "$body" | jget "['status']" || echo unknown; }
settled() { s="$(status "$1" "$2")"; [ "$s" = Sold ] || [ "$s" = Rejected ]; }
for _ in $(seq 1 240); do
  if settled "$A" "$IDA" && settled "$B" "$IDB" && settled "$C" "$IDC"; then break; fi
  sleep 1
done
SOLD=0; LOST=0
for pair in "$A|$IDA" "$B|$IDB" "$C|$IDC"; do
  s="$(status "${pair%%|*}" "${pair##*|}")"
  case "$s" in Sold) SOLD=$((SOLD + 1)) ;; Rejected) LOST=$((LOST + 1)) ;; *) fail "an order is stuck in $s" ;; esac
done
[ "$SOLD" = 1 ] && [ "$LOST" = 2 ] || fail "expected 1 sold and 2 not drawn, got $SOLD and $LOST"
[ "$(retry curl -sf -H "$ADM" "$API/api/raffles/RES-$RUN" | jget "['pairsLeft']")" = 0 ] || fail "the one pair must be held"
CLAIMS=0
for id in "$IDA" "$IDB" "$IDC"; do
  if curl -sf -H "$ADM" "$API/api/claims/$id" >/dev/null 2>&1; then CLAIMS=$((CLAIMS + 1)); fi
done
[ "$CLAIMS" = 1 ] || fail "exactly one claim may exist, found $CLAIMS"
retry curl -sf -H "$ADM" "$API/api/raffles/RES-$RUN/draw" | python3 -c "
import hashlib, json, sys
committed = sys.argv[1]
d = json.load(sys.stdin)
assert d['seedMatchesHash'] and hashlib.sha256(d['seed'].encode()).hexdigest() == committed
assert d['entries'] == 3 and d['winners'] == 1, d
rows = sorted(d['mine'], key=lambda e: e['key'])
assert [e['outcome'] for e in rows] == ['Won', 'Lost', 'Lost'], rows
" "$H0" || fail "the draw cannot be recomputed after the failures"
say "1 sold, 2 not drawn, 1 claim, the seed still matches its commitment, the winner is the lowest key"
echo "resilience passed"
