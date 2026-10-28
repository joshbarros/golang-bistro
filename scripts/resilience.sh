#!/usr/bin/env bash
# What Kubernetes buys: kill an API replica and the RabbitMQ pod in the middle of a booking and check that nothing is lost or
# done twice. Three parties ask for two covers each of a sitting that has four, all willing to wait: two are seated, one
# waits in the line, and when a seat is given back the party in the line is seated exactly once. Needs `make cluster-up`.
set -euo pipefail

API="${API:-http://127.0.0.1:5231}"
CTX="${CTX:-kind-golang-bistro}"
K="kubectl --context $CTX -n bistro"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'
say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }
# every call retries a few times: for a moment after a pod dies the Service may still send a request to it
retry() { for _ in $(seq 1 20); do if out="$("$@" 2>/dev/null)"; then printf '%s' "$out"; return 0; fi; sleep 1; done; return 1; }

login() { retry curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"admin@bistro.local\",\"password\":\"${ADMIN_PASSWORD:-BistroAdmin2026}\"}" | token_of; }
register() { retry curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"Resil-Passw0rd1\"}" | token_of; }
ADM="Authorization: Bearer $(login)"
A="Authorization: Bearer $(register a)"
B="Authorization: Bearer $(register b)"
C="Authorization: Bearer $(register c)"

echo "1. a sitting of four covers, and two parties of two in flight, both willing to wait"
retry curl -sf -X POST "$API/api/sittings" -H "$JSON" -H "$ADM" -d "{\"code\":\"RES-$RUN\",\"name\":\"Resilience\",\"room\":\"Terrace\",\"chef\":\"Chez Marlo\",\"coversTotal\":4,\"priceCents\":5000,\"maxPerGuest\":4}" >/dev/null || fail "could not open the sitting"
book() { retry curl -sf -X POST "$API/api/orders" -H "$JSON" -H "$1" -d "{\"sittingCode\":\"RES-$RUN\",\"reference\":\"RSV-$RUN-$2\",\"covers\":2,\"waitlist\":true}" | jget "['id']"; }
IDA="$(book "$A" a)"; IDB="$(book "$B" b)"

echo "2. kill an API replica and the RabbitMQ pod while the bookings are still in flight"
$K delete "$($K get pods -l app.kubernetes.io/name=api -o name | head -1)" --wait=false | sed 's/^/  /'
$K delete pod rabbitmq-0 --wait=false | sed 's/^/  /'
IDC="$(book "$C" c)" # accepted even now: the order and its outbox row are one local transaction
say "a third party was accepted while the broker was down"

echo "3. everything comes back: two parties are seated and confirmed, the third waits in the line, nothing is oversold"
$K rollout status statefulset/rabbitmq --timeout=180s >/dev/null
$K rollout status deployment/api --timeout=180s >/dev/null
status() { body="$(retry curl -sf -H "$1" "$API/api/orders/$2")" && printf '%s' "$body" | jget "['status']" || echo unknown; }
settled() { s="$(status "$1" "$2")"; [ "$s" = Sold ] || [ "$s" = Waitlisted ]; }
for _ in $(seq 1 240); do
  if settled "$A" "$IDA" && settled "$B" "$IDB" && settled "$C" "$IDC"; then break; fi
  sleep 1
done
SOLD=0; WAITING=0; WAITER=""; SEATED=""
for pair in "$A|$IDA" "$B|$IDB" "$C|$IDC"; do
  s="$(status "${pair%%|*}" "${pair##*|}")"
  case "$s" in Sold) SOLD=$((SOLD + 1)); SEATED="${SEATED:-$pair}" ;; Waitlisted) WAITING=$((WAITING + 1)); WAITER="$pair" ;; *) fail "an order is stuck in $s" ;; esac
done
[ "$SOLD" = 2 ] && [ "$WAITING" = 1 ] || fail "expected 2 seated and 1 waiting, got $SOLD and $WAITING"
[ "$(retry curl -sf -H "$ADM" "$API/api/sittings/RES-$RUN" | jget "['coversLeft']")" = 0 ] || fail "all four covers must be held, and no more"
say "2 seated, 1 waiting, 0 covers left"

echo "4. a seat is given back: the party in the line is seated, once"
retry curl -sf -X POST -H "${SEATED%%|*}" "$API/api/orders/${SEATED##*|}/refund" >/dev/null || fail "refund failed"
for _ in $(seq 1 120); do [ "$(status "${WAITER%%|*}" "${WAITER##*|}")" = Sold ] && break; sleep 1; done
[ "$(status "${WAITER%%|*}" "${WAITER##*|}")" = Sold ] || fail "the party in the line was not seated"
[ "$(status "${SEATED%%|*}" "${SEATED##*|}")" = Refunded ] || fail "the refunded order must be Refunded"
[ "$(retry curl -sf -H "$ADM" "$API/api/sittings/RES-$RUN" | jget "['coversLeft']")" = 0 ] || fail "after the promotion the sitting is full again, not overbooked"
say "the waiting party was seated; 0 covers left, no overbooking"
echo "resilience passed"
