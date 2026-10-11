#!/usr/bin/env bash
set -euo pipefail

: "${NOTIFY_URL:?Set NOTIFY_URL, for example https://notify.example.com}"
: "${NOTIFY_API_KEY:?Set NOTIFY_API_KEY}"

# Include API_PATH_PREFIX in NOTIFY_URL, for example https://notify.example.com/api/notify-qq.

# jq keeps quotes, newlines, and arbitrary script output valid in JSON.
content=${1:-"Script completed"}
button_url=${2:-""}
button_label=${3:-"Details"}
jq -n --arg content "$content" \
  --arg url "$button_url" --arg label "$button_label" \
  '{content: $content, buttons: (if $url == "" then [] else [{label: $label, url: $url}] end)}' |
  curl --fail-with-body --silent --show-error --max-time 25 \
    --request POST "${NOTIFY_URL%/}/notify" \
    --header "Authorization: Bearer ${NOTIFY_API_KEY}" \
    --header 'Content-Type: application/json' \
    --data-binary @-
