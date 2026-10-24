#!/usr/bin/env bash
# Measures System One latency: 10 runs each of a 3-question request with a
# short state and with a long state of about 30K tokens, near the largest
# TypeSafe's jev accepts (measured 2026-10-25: 32,865 input tokens answered,
# about 33.2K got 400 max_tokens_exceeded). The receipt behind attempt_timeout (C3).
#
#   scripts/latency.sh [url] [model]
#
# Defaults to TypeSafe directly, with TYPESAFE_API_KEY. Point it at brout
# (http://127.0.0.1:7979/v1/systemone, model "typesafe/jev-latest") to include
# brout's overhead; brout ignores the Authorization header.
set -euo pipefail
url=${1:-https://api.typesafe.ai/v1/systemone}
model=${2:-jev-latest}
runs=10
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

questions='{
  "positive": {"type": "noul", "instructions": "Is the customer happy overall?"},
  "topic": {"type": "choice", "instructions": "What is the main topic?", "criteria": {"billing": null, "shipping": null, "product": null}},
  "urgency": {"type": "score", "instructions": "How urgent is a reply?", "criteria": ["not urgent", "soon", "today", "now"]}
}'

printf '"Order 1042 arrived late and the box was damaged, but the product works."' > "$tmp/short"
# About 30K tokens of varied support-ticket text.
python3 - "$tmp/long" <<'PY'
import json, random, sys
random.seed(7)
words = "order shipping billing refund damaged box late arrived product works broken charged twice support agent email reply customer account invoice tracking courier warehouse replacement".split()
lines = [f"Ticket {i}: " + " ".join(random.choice(words) for _ in range(30)) + "." for i in range(820)]
open(sys.argv[1], "w").write(json.dumps("\n".join(lines)))
PY

for size in short long; do
  printf '{"model": "%s", "state": %s, "questions": %s}' "$model" "$(cat "$tmp/$size")" "$questions" > "$tmp/body"
  echo "== $size state"
  for i in $(seq $runs); do
    out=$(curl -sS -o "$tmp/resp" -w '%{http_code} %{time_total}' "$url" \
      -H "Authorization: Bearer ${TYPESAFE_API_KEY:-}" -H 'Content-Type: application/json' --data-binary @"$tmp/body")
    tokens=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("usage",{}).get("input_tokens","-"))' "$tmp/resp" 2>/dev/null || echo -)
    echo "run $i: status ${out% *}, ${out#* }s, input_tokens $tokens"
  done
done
