#!/usr/bin/env bash
# Drives the stack running in Kubernetes over HTTP. Every code carries a run id, so a kept Postgres volume never collides.
# Needs: curl, python3. The stack must be up: make cluster-up
set -euo pipefail

API="${API:-http://127.0.0.1:5228}"
RABBIT="${RABBIT:-http://127.0.0.1:15718}"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'

say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }

ADMIN_EMAIL="${ADMIN_EMAIL:-admin@sole.local}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-SoleAdmin2026}"
CUSTOMER_PASSWORD="Smoke-Passw0rd1"
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }

open_raffle() { # code pairsTotal [drawInSeconds=3600] -> prints nothing
  local at; at="$(python3 -c "import datetime,sys; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1]))).strftime('%Y-%m-%dT%H:%M:%S.%f')[:-3]+'Z')" "${3:-3600}")"
  curl -sf -X POST "$API/api/raffles" -H "$JSON" -H "$ADM" \
    -d "{\"code\":\"$1\",\"name\":\"Smoke Raffle\",\"colorway\":\"Volt\",\"brand\":\"Nimbus\",\"pairsTotal\":$2,\"drawAt\":\"$at\",\"priceCents\":5000}" >/dev/null \
    || fail "could not open raffle $1"
}
book() { # code reference pairs -> prints order id
  curl -sf -X POST "$API/api/orders" -H "$JSON" -H "${AS:-$CUS}" \
    -d "{\"raffleCode\":\"$1\",\"reference\":\"$2\",\"pairs\":$3}" | jget "['id']"
}
order_field() { curl -sf -H "$ADM" "$API/api/orders/$1" | jget "['$2']"; }
remaining() { curl -sf -H "$ADM" "$API/api/raffles/$1" | jget "['pairsLeft']"; }

wait_for() { # description command... : polls up to 30s until the command succeeds
  local what="$1"; shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  fail "timed out waiting for $what"
}
status_is() { [ "$(order_field "$1" status)" = "$2" ]; }
remaining_is() { [ "$(remaining "$1")" = "$2" ]; }
terminal() { case "$(order_field "$1" status)" in Sold|Rejected|Refunded) return 0 ;; *) return 1 ;; esac; }
entries_are() { [ "$(raffle_field "$1" entries)" = "$2" ]; }
raffle_field() { curl -sf -H "$ADM" "$API/api/raffles/$1" | jget "['$2']"; }
event_types() { curl -sf -H "$ADM" "$API/api/orders/$1/events" | python3 -c "import sys,json; print(' '.join(e['type'] for e in json.load(sys.stdin)))"; }
dlq_is() { [ "$(dlq_count)" = "$1" ]; }
dlq_count() { curl -sf -u platform:platform "$RABBIT/api/queues/%2F/claims.work.dlq" | jget "['messages']"; }

curl -sf "$API/api/health" >/dev/null || fail "API is not healthy at $API; run: make cluster-up"

echo "0. authentication and ownership"
[ "$(code_of "$API/api/raffles")" = 401 ] || fail "an anonymous request should be 401"
[ "$(code_of -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"wrong-Passw0rd\"}")" = 401 ] || fail "a wrong password should be 401"
ADM="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" | token_of)" || fail "the administrator could not sign in"
CUS="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"smoke-$RUN@example.com\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)" || fail "could not register a customer"
OTHER="Authorization: Bearer $(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"other-$RUN@example.com\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)"
[ "$(code_of -X POST "$API/api/raffles" -H "$JSON" -H "$CUS" -d '{"code":"X"}')" = 403 ] || fail "a customer must not open raffles"
open_raffle "A-$RUN" 5
IDA="$(book "A-$RUN" "ENT-$RUN-OWN" 1)"
wait_for "the entry to be recorded" status_is "$IDA" Entered
[ "$(code_of -H "$OTHER" "$API/api/orders/$IDA")" = 404 ] || fail "another customer must get 404 for someone else's order"
[ "$(code_of -H "$CUS" "$API/api/orders/$IDA/events")" = 403 ] || fail "the event log is staff only"
[ "$(code_of -X POST -H "$OTHER" "$API/api/orders/$IDA/refund")" = 404 ] || fail "another customer must not refund it"
say "401 anonymous, 401 bad password, 403 customer opening a raffle, 404 for another customer, 403 on the event log"

echo "0b. password reset ends the old sessions (needs the dev mailbox the demo stack turns on)"
RESET_EMAIL="reset-$RUN@example.com"
OLD="$(curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\",\"password\":\"$CUSTOMER_PASSWORD\"}" | token_of)"
[ "$(code_of -H "Authorization: Bearer $OLD" "$API/api/auth/me")" = 200 ] || fail "the new session should work"
[ "$(code_of -X POST "$API/api/auth/forgot-password" -H "$JSON" -d "{\"email\":\"nobody-$RUN@example.com\"}")" = 202 ] || fail "an unknown address must look the same"
curl -sf -X POST "$API/api/auth/forgot-password" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\"}" >/dev/null || fail "forgot-password failed"
LINK="$(curl -sf "$API/api/dev/mailbox?to=$RESET_EMAIL" | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['link'])")"
link_param() { python3 -c "import sys,urllib.parse as u; print(u.parse_qs(u.urlparse(sys.argv[1]).query)['$1'][0])" "$LINK"; }
[ "$(code_of -X POST "$API/api/auth/reset-password" -H "$JSON" -d "{\"userId\":\"$(link_param reset)\",\"token\":\"$(link_param token)\",\"newPassword\":\"Another-Passw0rd9\"}")" = 204 ] || fail "reset failed"
sleep 3 # the session version is cached for a moment
[ "$(code_of -H "Authorization: Bearer $OLD" "$API/api/auth/me")" = 401 ] || fail "the old access token should be revoked"
[ "$(code_of -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"$RESET_EMAIL\",\"password\":\"Another-Passw0rd9\"}")" = 200 ] || fail "the new password should work"
say "reset link works once, the old access token is revoked, the new password signs in"

echo "1. input is validated at the door"
open_raffle "V-$RUN" 10
for body in \
  "{\"raffleCode\":\"V-$RUN\",\"reference\":\"\",\"pairs\":1}" \
  "{\"raffleCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"pairs\":0}" \
  "{\"raffleCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"pairs\":7}" \
  "{\"raffleCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"pairs\":4294967297}" \
  "{\"raffleCode\":\"V-$RUN\",\"reference\":\"R-$RUN\",\"pairs\":9223372036854875807}"; do
  code="$(code_of -X POST "$API/api/orders" -H "$JSON" -H "$CUS" -d "$body")"
  [ "$code" = 400 ] || fail "expected 400 for $body, got $code"
done
[ "$(code_of -X POST "$API/api/raffles" -H "$JSON" -H "$ADM" -d "{\"code\":\"Z-$RUN\",\"name\":\"x\",\"colorway\":\"x\",\"brand\":\"x\",\"pairsTotal\":3,\"drawAt\":\"2001-01-01T00:00:00Z\",\"priceCents\":5000}")" = 400 ] || fail "a draw time in the past must be refused"
say "empty, zero, over-limit and overflowing pair counts are all 400, and so is a draw time in the past"

echo "2. happy path: enter, the draw runs by itself, the winner is sold"
open_raffle "H-$RUN" 20 6
H0="$(raffle_field "H-$RUN" seedHash)"
ID="$(book "H-$RUN" "ENT-$RUN-1" 4)"
wait_for "the entry to be recorded" status_is "$ID" Entered
remaining_is "H-$RUN" 20 || fail "entering must not take pairs"
wait_for "order to be Sold" status_is "$ID" Sold
[ "$(order_field "$ID" totalCents)" = 22400 ] || fail "total should be 4 x 5000 plus a 12% service fee = 22400"
[ "$(curl -sf -H "$ADM" "$API/api/claims/$ID" | jget "['status']")" = Issued ] || fail "claim not issued"
wait_for "remaining pairs to be 16" remaining_is "H-$RUN" 16
[ "$(raffle_field "H-$RUN" status)" = Drawn ] || fail "the raffle should be drawn"
python3 - "$(raffle_field "H-$RUN" seed)" "$H0" <<'PY' || fail "the revealed seed does not match the committed hash"
import hashlib, sys
seed, committed = sys.argv[1], sys.argv[2]
assert hashlib.sha256(seed.encode()).hexdigest() == committed, (seed, committed)
PY
TYPES="$(event_types "$ID")"
for t in pairs.reserve pairs.entered pairs.held claim.issued; do
  case "$TYPES" in *"$t"*) ;; *) fail "Kafka log is missing $t (has: $TYPES)" ;; esac
done
say "entered with 20 left, drawn by itself, sold at 22400, 16 left, the seed matches its commitment, Kafka log has the four steps"

echo "3. the draw: one pair, three entrants: one wins, two are told they were not drawn, and anyone can recompute it"
open_raffle "L-$RUN" 1 8
L0="$(raffle_field "L-$RUN" seedHash)"
IDS=""
for n in A B C; do
  case $n in A) WHO="$ADM" ;; B) WHO="$CUS" ;; C) WHO="$OTHER" ;; esac
  IDS="$IDS $(AS="$WHO" book "L-$RUN" "ENT-$RUN-$n" 1)"
done
for i in $IDS; do wait_for "entry $i to be recorded" status_is "$i" Entered; done
for i in $IDS; do wait_for "order $i to settle" terminal "$i"; done
SOLD=0; LOST=0
for i in $IDS; do
  s="$(order_field "$i" status)"
  if [ "$s" = Sold ]; then SOLD=$((SOLD + 1)); fi
  if [ "$s" = Rejected ]; then
    case "$(order_field "$i" reason)" in "Not drawn"*) LOST=$((LOST + 1)); case "$(event_types "$i")" in *pairs.held*) fail "a loser has a hold" ;; esac ;; esac
  fi
done
[ "$SOLD" = 1 ] && [ "$LOST" = 2 ] || fail "expected 1 sold and 2 not drawn, got $SOLD sold and $LOST not drawn"
[ "$(remaining "L-$RUN")" = 0 ] || fail "remaining should be 0"
curl -sf -H "$ADM" "$API/api/raffles/L-$RUN/draw" | python3 -c "
import hashlib, json, sys
committed = sys.argv[1]
d = json.load(sys.stdin)
assert d['seedMatchesHash'] and hashlib.sha256(d['seed'].encode()).hexdigest() == committed
assert d['entries'] == 3 and d['winners'] == 1, d
rows = sorted(d['mine'], key=lambda e: e['key'])
for e in rows: assert hashlib.sha256((d['seed'] + ':' + e['orderId']).encode()).hexdigest() == e['key']
assert [e['outcome'] for e in rows] == ['Won', 'Lost', 'Lost'], rows  # one pair, first fit in key order
" "$L0" || fail "the draw cannot be recomputed from the seed"
say "1 sold, 2 not drawn, remaining 0, recomputing SHA-256(seed:orderId) gives the same winner"

echo "3b. one entry per person, withdrawing, and entries after the draw time"
open_raffle "D-$RUN" 3
D1="$(AS="$CUS" book "D-$RUN" "ENT-$RUN-D1" 1)"
wait_for "the first entry" status_is "$D1" Entered
[ "$(code_of -X POST "$API/api/orders" -H "$JSON" -H "$CUS" -d "{\"raffleCode\":\"D-$RUN\",\"reference\":\"ENT-$RUN-D1\",\"pairs\":1}")" = 409 ] || fail "the same reference twice must be a 409"
D2="$(AS="$CUS" book "D-$RUN" "ENT-$RUN-D2" 1)"
wait_for "the second entry by the same person to settle" terminal "$D2"
[ "$(order_field "$D2" status)" = Rejected ] || fail "a second entry by the same person must be rejected"
case "$(order_field "$D2" reason)" in "Already entered"*) ;; *) fail "the reason should be one-entry-per-person, got: $(order_field "$D2" reason)" ;; esac
[ "$(raffle_field "D-$RUN" entries)" = 1 ] || fail "only one entry should exist"
[ "$(code_of -X POST -H "$OTHER" "$API/api/orders/$D1/refund")" = 404 ] || fail "only the owner can withdraw"
curl -sf -X POST -H "$CUS" "$API/api/orders/$D1/refund" >/dev/null || fail "withdrawing an entry failed"
wait_for "the entry to be withdrawn" status_is "$D1" Refunded
wait_for "the entry count to drop" entries_are "D-$RUN" 0
open_raffle "E-$RUN" 3 3
sleep 5
[ "$(code_of -X POST "$API/api/orders" -H "$JSON" -H "$CUS" -d "{\"raffleCode\":\"E-$RUN\",\"reference\":\"ENT-$RUN-E1\",\"pairs\":1}")" = 409 ] || fail "entering after the draw time must be a 409"
say "a second reference is 409, a second entry by the same person is refused, a withdrawal removes the entry, a late entry is 409"

echo "4. poison: it wins the draw, is committed as rejected, dead-lettered, and the pairs come back"
open_raffle "P-$RUN" 5 5
BEFORE="$(dlq_count)"
IDP="$(book "P-$RUN" "ENT-POISON-$RUN" 2)"
wait_for "poison order to be Rejected" status_is "$IDP" Rejected
wait_for "pairs to be restored to 5" remaining_is "P-$RUN" 5
wait_for "claims.work.dlq to gain one message" dlq_is $((BEFORE + 1))
[ "$(curl -sf -H "$ADM" "$API/api/claims/$IDP" | jget "['status']")" = Rejected ] || fail "rejection should be committed on the claim row"
[ "$(order_field "$IDP" pairsReleasedAt)" != None ] || fail "order should record the release"
say "rejected, dead-lettered (+1 in claims.work.dlq), remaining back to 5"

echo "4b. dead letters can be listed by staff only, and replayed"
[ "$(code_of -H "$CUS" "$API/api/admin/dead-letters")" = 403 ] || fail "dead letters are staff only"
curl -sf -H "$ADM" "$API/api/admin/dead-letters" | python3 -c "import sys,json; q={x['queue']:x for x in json.load(sys.stdin)}; assert q['claims.work']['count']>=1, q" || fail "the dead-letter queue should list the poison message"
BEFORE_REPLAY="$(dlq_count)"
REPLAYED="$(curl -sf -X POST -H "$ADM" "$API/api/admin/dead-letters/claims.work/replay?max=100" | jget "['replayed']")" || fail "replay failed"
[ "$REPLAYED" -ge 1 ] || fail "replay should have taken at least one message"
# The poison reference still fails for the same reason, so it is retried and dead-lettered again: replay is for fixed causes.
wait_for "the replayed poison message to be dead-lettered again" dlq_is "$BEFORE_REPLAY"
say "listed, replayed $REPLAYED, dead-lettered again because the cause is still there"

echo "5. refund races the final mark: the refund wins, the issued claim stays"
WON=0
for attempt in 1 2 3; do
  RR="R$attempt-$RUN"
  open_raffle "$RR" 6 4
  IDR="$(AS="$CUS" book "$RR" "ENT-$RUN-R$attempt" 2)"
  # The claim exists before the order is marked Sold (the final mark is delayed in the demo cluster).
  wait_for "claim to exist" curl -sf -H "$ADM" "$API/api/claims/$IDR"
  if status_is "$IDR" Held; then WON=1; break; fi
  say "window missed on attempt $attempt (order already Sold); trying a fresh raffle"
done
[ "$WON" = 1 ] || fail "never caught the order between claim issued and final mark"
curl -sf -X POST -H "$CUS" "$API/api/orders/$IDR/refund" >/dev/null || fail "refund was refused"
wait_for "order to be Refunded" status_is "$IDR" Refunded
sleep 3 # longer than the final-mark delay: the late mark must not undo the refund
status_is "$IDR" Refunded || fail "the final mark overwrote the refund"
[ "$(curl -sf -H "$ADM" "$API/api/claims/$IDR" | jget "['status']")" = Issued ] || fail "the issued claim was rolled back"
[ "$(order_field "$IDR" claimNumber)" = None ] || fail "the late final mark should have been skipped"
wait_for "pairs to come back" remaining_is "$RR" 6
code="$(code_of -X POST -H "$CUS" "$API/api/orders/$IDR/refund")"
[ "$code" = 409 ] || fail "second refund should be 409, got $code"
say "order Refunded, claim still Issued, remaining 6, second refund 409"

echo "golang-sole smoke passed"
