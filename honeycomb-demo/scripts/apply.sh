#!/usr/bin/env bash
# apply.sh — Full one-shot deploy of the Honeycomb demo stack to Kubernetes.
# Prerequisites: kubectl, a running cluster (kind/minikube/EKS/GKE), Docker (for building images).
#
# Usage:
#   HONEYCOMB_API_KEY=hcaik_... ./scripts/apply.sh

set -euo pipefail

NAMESPACE="url-shortener"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UPSTREAM_REPO="https://github.com/piyushsachdeva/microservice-url-shortener.git"
BUILD_DIR="/Users/piyus/Documents/honeycomb-demo/url-shortener-build"

if [[ -z "${HONEYCOMB_API_KEY:-}" ]]; then
  echo "❌ HONEYCOMB_API_KEY is not set."
  echo "   Export it first:  export HONEYCOMB_API_KEY=hcaik_..."
  exit 1
fi

# ── Step 1: Clone upstream and apply OTel patches ──────────────────────────
echo "📦 Cloning upstream repo..."
rm -rf "${BUILD_DIR}"
git clone --depth=1 "${UPSTREAM_REPO}" "${BUILD_DIR}"

echo "🔧 Applying OTel patches..."
# Copy OTel init package
mkdir -p "${BUILD_DIR}/internal/otel"
cp "${REPO_ROOT}/patches/otel/otel.go" "${BUILD_DIR}/internal/otel/"

# Replace service main.go files with instrumented versions
cp "${REPO_ROOT}/patches/services/link-service/main.go"     "${BUILD_DIR}/services/link-service/main.go"
cp "${REPO_ROOT}/patches/services/redirect-service/main.go" "${BUILD_DIR}/services/redirect-service/main.go"
cp "${REPO_ROOT}/patches/services/stats-service/main.go"    "${BUILD_DIR}/services/stats-service/main.go"

# Fix: return [] instead of null when no links exist (upstream bug)
mkdir -p "${BUILD_DIR}/internal/adapters/repository/postgres"
cp "${REPO_ROOT}/patches/internal/adapters/repository/postgres/link.go" \
   "${BUILD_DIR}/internal/adapters/repository/postgres/link.go"

# Add OTel dependencies to go.mod
cd "${BUILD_DIR}"
go get go.opentelemetry.io/otel@v1.27.0
go get go.opentelemetry.io/otel/sdk@v1.27.0
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.27.0
go get go.opentelemetry.io/otel/semconv/v1.21.0@v1.27.0
go get go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin@v0.52.0
go mod tidy

# ── Step 2: Build Docker images ────────────────────────────────────────────
echo "🐳 Building Docker images..."
REGISTRY="${REGISTRY:-piyushsachdeva}"   # override with your own registry

# --platform linux/amd64 ensures images run on EKS amd64 nodes regardless of build machine (Mac M1/M2, arm64 Linux etc)
# --no-cache prevents stale arm64 layers from being reused when switching platforms
docker build --platform linux/amd64 --no-cache -t "${REGISTRY}/url-shortener-link:otel"     -f services/link-service/Dockerfile     .
docker build --platform linux/amd64 --no-cache -t "${REGISTRY}/url-shortener-redirect:otel" -f services/redirect-service/Dockerfile .
docker build --platform linux/amd64 --no-cache -t "${REGISTRY}/url-shortener-stats:otel"    -f services/stats-service/Dockerfile    .

# If using kind, load images directly (no push needed)
if kubectl config current-context 2>/dev/null | grep -q kind; then
  echo "🚢 Loading images into kind cluster..."
  kind load docker-image "${REGISTRY}/url-shortener-link:otel"
  kind load docker-image "${REGISTRY}/url-shortener-redirect:otel"
  kind load docker-image "${REGISTRY}/url-shortener-stats:otel"
else
  echo "🚢 Pushing images to registry..."
  docker push "${REGISTRY}/url-shortener-link:otel"
  docker push "${REGISTRY}/url-shortener-redirect:otel"
  docker push "${REGISTRY}/url-shortener-stats:otel"
fi

# ── Step 3: Ensure ingress-nginx is installed ──────────────────────────────
cd "${REPO_ROOT}"
if ! kubectl get deployment ingress-nginx-controller -n ingress-nginx &>/dev/null; then
  echo "📥 Installing ingress-nginx..."
  kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.10.1/deploy/static/provider/aws/deploy.yaml
  kubectl rollout status deployment/ingress-nginx-controller -n ingress-nginx --timeout=120s
  kubectl patch configmap ingress-nginx-controller -n ingress-nginx \
    --type merge -p '{"data":{"allow-snippet-annotations":"true"}}'
  kubectl rollout restart deployment/ingress-nginx-controller -n ingress-nginx
  kubectl rollout status deployment/ingress-nginx-controller -n ingress-nginx --timeout=60s
  echo "✅ ingress-nginx ready"
else
  echo "✅ ingress-nginx already installed"
fi

# ── Step 4: Apply K8s manifests ────────────────────────────────────────────
echo "☸️  Applying K8s manifests..."

kubectl apply -f k8s/00-namespace.yaml

# Create the Honeycomb secret from env var (not from the template file)
kubectl create secret generic honeycomb-secret \
  --namespace="${NAMESPACE}" \
  --from-literal=api-key="${HONEYCOMB_API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

# Create postgres secret
kubectl create secret generic postgres-secret \
  --namespace="${NAMESPACE}" \
  --from-literal=username=postgres \
  --from-literal=password=postgres \
  --from-literal=database=urlshortener \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f k8s/02-postgres.yaml
kubectl apply -f k8s/03-redis.yaml
kubectl apply -f k8s/04-otel-collector.yaml
kubectl apply -f k8s/05-feature-flags.yaml
kubectl apply -f k8s/10-frontend.yaml
kubectl apply -f k8s/06-link-service.yaml
kubectl apply -f k8s/07-redirect-service.yaml
kubectl apply -f k8s/08-stats-service.yaml
kubectl apply -f k8s/09-nginx.yaml
kubectl apply -f k8s/11-ingress.yaml

echo ""
echo "⏳ Waiting for all deployments to be ready..."
kubectl rollout status deployment/otel-collector   -n "${NAMESPACE}" --timeout=120s
kubectl rollout status deployment/link-service     -n "${NAMESPACE}" --timeout=120s
kubectl rollout status deployment/redirect-service -n "${NAMESPACE}" --timeout=120s
kubectl rollout status deployment/stats-service    -n "${NAMESPACE}" --timeout=120s
kubectl rollout status deployment/nginx-gateway    -n "${NAMESPACE}" --timeout=60s

# ── Resolve app URL ────────────────────────────────────────────────────────
# Try ingress-nginx LoadBalancer first (EKS), fall back to NodePort
LB_HOST=$(kubectl get svc ingress-nginx-controller -n ingress-nginx \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)

if [[ -n "${LB_HOST}" ]]; then
  APP_URL="http://${LB_HOST}"
else
  NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)
  APP_URL="http://${NODE_IP:-localhost}:30080"
fi

echo ""
echo "✅ Stack is live!"
echo ""
echo "   App URL  : ${APP_URL}"
echo "   Pods     :"
kubectl get pods -n "${NAMESPACE}" --no-headers | awk '{printf "     %-45s %s\n", $1, $3}'
echo ""
echo "   Quick smoke test:"
echo "     export APP_URL=\"${APP_URL}\""
echo "     curl -s -X PUT \"\${APP_URL}/api/generate\" -H 'Content-Type: application/json' \\"
echo "          -d '{\"url\":\"https://honeycomb.io\"}' | jq ."
echo ""
echo "   Next steps:"
echo "     1. Open Honeycomb — traces should appear within ~30s"
echo "     2. Start traffic  : ./scripts/load-test.sh \"\${APP_URL}\""
echo "     3. Inject chaos   : ./scripts/inject-chaos.sh 800"
echo "     4. Watch Honeycomb AI investigate the SLO burn"
echo "     5. Remove chaos   : ./scripts/remove-chaos.sh"
