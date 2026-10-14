#!/usr/bin/env bash
# Measure how many input tokens a /v1/systemone request costs, and where they go.
#
#   scripts/measure-tokens.sh [-c config.json] [-m model] [-u ollama-url]
#
# Needs: curl, jq, and either bin/tev1-rank (to print the default config) or -c.
# It sends 8 small requests, so on a slow iGPU expect a few minutes.
#
# It answers three questions:
#   1. What is the FIXED cost (all questions, an almost-empty state)?
#   2. How much of that does each question contribute (or is it a flat template cost)?
#   3. How many tokens does one byte of segment text cost, and so how many bytes of
#      segment fit in the context window?
set -euo pipefail

CONFIG="" MODEL="" URL="${OLLAMA_HOST:-http://localhost:11434}" CTX=2048
while getopts "c:m:u:x:" o; do
  case $o in c) CONFIG=$OPTARG ;; m) MODEL=$OPTARG ;; u) URL=$OPTARG ;; x) CTX=$OPTARG ;; *) exit 2 ;; esac
done
case "$URL" in http*) ;; *) URL="http://$URL" ;; esac
cd "$(dirname "$0")/.."

if [[ -n $CONFIG ]]; then CFG=$(cat "$CONFIG"); else CFG=$(./bin/tev1-rank config); fi
# A config file may be a partial override; fall back to defaults for anything missing.
if [[ -n $CONFIG ]]; then
  DEF=$(./bin/tev1-rank config)
  CFG=$(jq -s '.[0] * .[1] | .questions = (.[1].questions // .[0].questions)' <(echo "$DEF") <(echo "$CFG") 2>/dev/null || echo "$CFG")
fi
[[ -n $MODEL ]] || MODEL=$(jq -r .model <<<"$CFG")
QS=$(jq -c '.questions | map({(.name): {type, instructions, criteria}}) | add' <<<"$CFG")

probe() { # probe <questions-json> <state-string> -> input_tokens
  jq -n --arg m "$MODEL" --argjson q "$1" --arg s "$2" '{model:$m, state:$s, questions:$q}' |
    curl -fsS -m 900 "$URL/v1/systemone" -d @- | jq -r '.usage.input_tokens'
}

SHORT="x"
TXT200=$(printf 'The committee reviewed the annual figures and agreed on the revised budget for the coming year. %.0s' 1 | head -c 200)
TXT800=$(for i in 1 2 3 4; do printf '%s ' "$TXT200"; done | head -c 800)

echo "model: $MODEL   ollama: $URL   assumed context: $CTX tokens"
echo
all_short=$(probe "$QS" "$SHORT");  echo "all questions, 1-byte state : $all_short"
sum_single=0
for name in $(jq -r '.questions[].name' <<<"$CFG"); do
  one=$(jq -c --arg n "$name" '{($n): .[$n]}' <<<"$QS")
  t=$(probe "$one" "$SHORT"); sum_single=$((sum_single + t))
  printf '  question %-14s alone     : %s\n' "$name" "$t"
done
echo "  sum of questions alone       : $sum_single   (≈ all-questions figure ⇒ every question is a separate pass over the prompt)"
t200=$(probe "$QS" "$TXT200"); echo "all questions, 200-byte state: $t200"
t800=$(probe "$QS" "$TXT800"); echo "all questions, 800-byte state: $t800"

awk -v a="$all_short" -v b="$t200" -v c="$t800" -v ctx="$CTX" 'BEGIN{
  per=(c-b)/600.0; if (per<=0) per=0.0001
  room=(ctx-a)/per
  printf "\nfixed cost (questions + template): %d tokens\n", a
  printf "cost per byte of segment text    : %.2f tokens  (%.0f tokens per 1000 bytes)\n", per, per*1000
  if (room<=0) printf "=> the fixed cost alone is >= %d: even an empty segment overflows the context.\n", ctx
  else printf "=> about %d bytes of state text fit in a %d-token context.\n", room, ctx
}'
