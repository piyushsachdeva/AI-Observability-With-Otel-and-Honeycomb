#!/usr/bin/env bash
# remove-chaos.sh — Removes all chaos injections and restores normal operation.
# Use this for "the reveal" at the end of the demo.

set -euo pipefail

NAMESPACE="url-shortener"

echo "🩹 Removing chaos from redirect-service..."

kubectl patch configmap feature-flags \
  --namespace="${NAMESPACE}" \
  --type merge \
  --patch '{"data":{"CHAOS_LATENCY_MS":"0","CHAOS_STATS_ERROR_RATE":"0"}}'

kubectl rollout restart deployment/redirect-service --namespace="${NAMESPACE}"
kubectl rollout status deployment/redirect-service --namespace="${NAMESPACE}" --timeout=60s

echo ""
echo "✅ Chaos removed. SLO should start recovering in Honeycomb within ~30 seconds."
