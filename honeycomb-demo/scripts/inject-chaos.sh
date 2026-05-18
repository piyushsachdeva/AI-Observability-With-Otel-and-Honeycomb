#!/usr/bin/env bash
# inject-chaos.sh — Injects artificial latency into the redirect-service.
# Mimics a real production degradation (e.g. a slow downstream dependency).
# Honeycomb's trigger should fire within ~2 minutes as latency spikes.
#
# Usage:
#   ./scripts/inject-chaos.sh           # default: 800ms latency
#   ./scripts/inject-chaos.sh 1500      # custom latency in ms

set -euo pipefail

LATENCY_MS="${1:-800}"
NAMESPACE="url-shortener"

echo "🔥 Injecting ${LATENCY_MS}ms latency into redirect-service..."

kubectl patch configmap feature-flags \
  --namespace="${NAMESPACE}" \
  --type merge \
  --patch "{\"data\":{\"CHAOS_LATENCY_MS\":\"${LATENCY_MS}\"}}"

echo "✅ ConfigMap patched. Rolling redirect-service pods to pick up new env..."

# Force pod restart so envFrom picks up the new value immediately
kubectl rollout restart deployment/redirect-service --namespace="${NAMESPACE}"
kubectl rollout status deployment/redirect-service --namespace="${NAMESPACE}" --timeout=60s

echo ""
echo "🚨 Chaos is live! redirect-service now adds ${LATENCY_MS}ms to every redirect."
echo "   Watch the latency spike in Honeycomb — the Slack alert should fire within 1-2 min."
echo ""
echo "   To stop chaos: ./scripts/remove-chaos.sh"
