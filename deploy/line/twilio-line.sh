#!/bin/bash
# Give a new phone line its own Twilio credentials and number, by API, with no new Twilio account:
# a subaccount of the main account (its own SID, auth token and API key, so the line shares no
# credential with any other line), plus one US local number bought in it (about $1.15 a month).
#
#   TWILIO_MAIN_SID=AC… TWILIO_MAIN_AUTH_TOKEN=… twilio-line.sh <subaccount name> <ssh host> <secrets file on the host> [US area code]
#
# Idempotent: an existing subaccount of that name and an existing number in it are reused; a new
# API key is made only when the secrets file has none. The four TWILIO_* values go straight into
# the host's secrets file (root 600); none is printed and none is kept on this machine. Twilio can
# show the subaccount's auth token again; an API key's secret is shown once, so losing the file
# means making a new key (delete the old one in the console).
#
# Know before you use it:
# - A subaccount draws on the MAIN account's prepaid balance. The line's own limit
#   (policy.limits.spend_per_month_usd) is an estimate; the shared balance, with auto-recharge
#   off, is the hard ceiling for every line together. A line that needs its own ceiling needs
#   its own Twilio account.
# - Buying the number spends money: get the owner's yes first.
# - Off switch for one line's Twilio side: suspend its subaccount (POST Status=suspended).
set -euo pipefail
NAME="${1:?subaccount name}"; HOST="${2:?ssh host}"; FILE="${3:?secrets file on the host}"; AREA="${4:-}"
# The main account's SID and auth token, from the environment (never a file in the repo).
MAIN_SID="${TWILIO_MAIN_SID:?set TWILIO_MAIN_SID to the main Twilio account SID}"
MAIN_TOK="${TWILIO_MAIN_AUTH_TOKEN:?set TWILIO_MAIN_AUTH_TOKEN to the main account auth token}"
API=https://api.twilio.com/2010-04-01
tw() { curl -sS -m 30 --fail-with-body "$@"; }
set_secret() { # set_secret KEY, value on stdin; replaces the key in the host's file
  ssh "$HOST" "sudo -n sh -c 'umask 077; mkdir -p \$(dirname $FILE); touch $FILE; v=\$(cat); { grep -v \"^$1=\" $FILE || true; printf \"%s=%s\n\" $1 \"\$v\"; } > $FILE.new && mv $FILE.new $FILE && chmod 600 $FILE'"
}

sub="$(tw -u "$MAIN_SID:$MAIN_TOK" -G "$API/Accounts.json" --data-urlencode "FriendlyName=$NAME" --data-urlencode "Status=active")"
SUB_SID="$(jq -r --arg m "$MAIN_SID" '[.accounts[] | select(.sid != $m)][0].sid // empty' <<<"$sub")"
if [ -z "$SUB_SID" ]; then
  sub="$(tw -u "$MAIN_SID:$MAIN_TOK" -X POST "$API/Accounts.json" --data-urlencode "FriendlyName=$NAME")"
  SUB_SID="$(jq -r .sid <<<"$sub")"; SUB_TOK="$(jq -r .auth_token <<<"$sub")"
  echo "subaccount created: $NAME $SUB_SID"
else
  SUB_TOK="$(jq -r --arg s "$SUB_SID" '.accounts[] | select(.sid == $s) | .auth_token' <<<"$sub")"
  echo "subaccount exists: $NAME $SUB_SID"
fi
[ -n "$SUB_TOK" ] && [ "$SUB_TOK" != null ] || { echo "no auth token for $SUB_SID" >&2; exit 1; }
printf %s "$SUB_SID" | set_secret TWILIO_ACCOUNT_SID
printf %s "$SUB_TOK" | set_secret TWILIO_AUTH_TOKEN

if ssh "$HOST" "sudo -n grep -q '^TWILIO_API_KEY_SECRET=.' $FILE"; then
  echo "API key: already in $FILE"
else
  key="$(tw -u "$SUB_SID:$SUB_TOK" -X POST "$API/Accounts/$SUB_SID/Keys.json" --data-urlencode "FriendlyName=agent-phone $NAME")"
  KEY_SID="$(jq -r .sid <<<"$key")"
  [ -n "$KEY_SID" ] && [ "$KEY_SID" != null ] || { echo "API key not created" >&2; exit 1; }
  printf %s "$KEY_SID" | set_secret TWILIO_API_KEY_SID
  jq -j .secret <<<"$key" | set_secret TWILIO_API_KEY_SECRET
  echo "API key created: $KEY_SID (secret stored on $HOST only)"
fi

NUM="$(tw -u "$SUB_SID:$SUB_TOK" "$API/Accounts/$SUB_SID/IncomingPhoneNumbers.json" | jq -r '.incoming_phone_numbers[0].phone_number // empty')"
if [ -n "$NUM" ]; then
  echo "number exists: $NUM"
else
  PICK="$(tw -u "$SUB_SID:$SUB_TOK" -G "$API/Accounts/$SUB_SID/AvailablePhoneNumbers/US/Local.json" ${AREA:+-d AreaCode="$AREA"} -d VoiceEnabled=true -d PageSize=1 | jq -r '.available_phone_numbers[0].phone_number // empty')"
  [ -n "$PICK" ] || PICK="$(tw -u "$SUB_SID:$SUB_TOK" -G "$API/Accounts/$SUB_SID/AvailablePhoneNumbers/US/Local.json" -d VoiceEnabled=true -d PageSize=1 | jq -r '.available_phone_numbers[0].phone_number // empty')"
  [ -n "$PICK" ] || { echo "no US local number available" >&2; exit 1; }
  NUM="$(tw -u "$SUB_SID:$SUB_TOK" -X POST "$API/Accounts/$SUB_SID/IncomingPhoneNumbers.json" --data-urlencode "PhoneNumber=$PICK" --data-urlencode "FriendlyName=agent-phone $NAME" | jq -r .phone_number)"
  [ -n "$NUM" ] && [ "$NUM" != null ] || { echo "number not bought" >&2; exit 1; }
  echo "number bought: $NUM"
fi
echo "done: put $NUM in the line's dialout.yaml (twilio.from and the Self allowlist entry)"
