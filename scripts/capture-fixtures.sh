#!/usr/bin/env bash
# Capture real /v1/systemone responses for the fixtures in testdata/api/.
# Usage: scripts/capture-fixtures.sh [ollama-url]   (default http://localhost:11434)
set -euo pipefail
URL="${1:-${OLLAMA_HOST:-http://localhost:11434}}"
case "$URL" in http*) ;; *) URL="http://$URL" ;; esac
cd "$(dirname "$0")/../testdata/api"

echo "Ollama version:"; curl -fsS "$URL/api/version" | tee ollama-version.json; echo
for name in basic classify; do
  echo "capturing $name ..."
  curl -fsS "$URL/v1/systemone" -H 'Content-Type: application/json' \
    -d @"$name.request.json" -o "$name.response.json"
  if command -v jq >/dev/null; then jq . "$name.response.json" > "$name.response.json.tmp" && mv "$name.response.json.tmp" "$name.response.json"; fi
done
rm -f SYNTHETIC
echo "done. Review the diff (git diff testdata/api) and run: go test ./..."
