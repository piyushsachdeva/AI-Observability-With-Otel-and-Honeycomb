#!/usr/bin/env bash
# load-test.sh — Generates continuous traffic against the URL shortener.
# Run this BEFORE injecting chaos so the SLO has baseline data, and keep it
# running throughout the demo so Honeycomb sees enough events to trigger investigation.
#
# Usage:
#   ./scripts/load-test.sh                        # targets localhost:30080 (NodePort default)
#   ./scripts/load-test.sh http://your-lb-ip      # custom base URL
#   ./scripts/load-test.sh "" 50                  # 50 concurrent workers

set -euo pipefail

BASE_URL="${1:-http://localhost:30080}"
WORKERS="${2:-10}"
NAMESPACE="url-shortener"

# Confirm the app is reachable before hammering it
if ! curl -sf "${BASE_URL}/api/stats/health" &>/dev/null; then
  echo "⚠️  Cannot reach ${BASE_URL} — is the cluster running?"
  exit 1
fi

echo "🚦 Load test started"
echo "   Base URL : ${BASE_URL}"
echo "   Workers  : ${WORKERS}"
echo "   Press Ctrl+C to stop."
echo ""

# Seed a few short links so redirects have targets
echo "🌱 Seeding short links..."
URLS=(
  "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
  "https://github.com/piyushsachdeva/microservice-url-shortener"
  "https://docs.honeycomb.io/get-started/start-building/application/"
  "https://opentelemetry.io/docs/instrumentation/go/"
  "https://www.cncf.io/projects/opentelemetry/"
)

SHORT_IDS=()
for url in "${URLS[@]}"; do
  response=$(curl -sf -X PUT "${BASE_URL}/api/generate" \
    -H "Content-Type: application/json" \
    -d "{\"long\":\"${url}\"}" 2>/dev/null || echo "{}")
  id=$(echo "${response}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || true)
  if [[ -n "${id}" ]]; then
    SHORT_IDS+=("${id}")
    echo "   Created: ${id} → ${url:0:60}..."
  fi
done

if [[ ${#SHORT_IDS[@]} -eq 0 ]]; then
  echo "⚠️  Could not create any short links. Check the link-service logs."
  exit 1
fi

echo ""
echo "🔁 Running traffic loop with ${WORKERS} workers (Ctrl+C to stop)..."

worker() {
  local ids=("$@")
  local n=${#ids[@]}
  while true; do
    local id="${ids[$((RANDOM % n))]}"
    # Redirect (the hot path — this is what the trigger measures)
    curl -sf -o /dev/null -w "" --max-time 5 \
      "${BASE_URL}/r/${id}" 2>/dev/null || true
    # Occasionally check stats
    if (( RANDOM % 10 == 0 )); then
      curl -sf -o /dev/null --max-time 5 \
        "${BASE_URL}/api/stats/${id}" 2>/dev/null || true
    fi
    sleep "0.0$(( RANDOM % 5 + 1 ))"
  done
}

# Export so subshells can use it
export BASE_URL
export -f worker

for (( i=0; i<WORKERS; i++ )); do
  worker "${SHORT_IDS[@]}" &
done

wait
