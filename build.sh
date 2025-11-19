#!/usr/bin/env bash

set -euo pipefail
set +H

BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

info() { echo -e "${BLUE}[INFO]${NC} $1"; }
success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; }

APP_NAME=${APP_NAME:-"pos-service"}
NAMESPACE=${NAMESPACE:-"pos"}
ENV_SECRET_NAME=${ENV_SECRET_NAME:-"pos-service-secrets"}
DEPLOY=${DEPLOY:-true}
SETUP_DATABASES=${SETUP_DATABASES:-true}
DB_TYPES=${DB_TYPES:-postgres,redis}
# Per-service database configuration
SERVICE_DB_NAME=${SERVICE_DB_NAME:-pos}
SERVICE_DB_USER=${SERVICE_DB_USER:-pos_user}

REGISTRY_SERVER=${REGISTRY_SERVER:-docker.io}
REGISTRY_NAMESPACE=${REGISTRY_NAMESPACE:-codevertex}
IMAGE_REPO="${REGISTRY_SERVER}/${REGISTRY_NAMESPACE}/${APP_NAME}"

DEVOPS_REPO=${DEVOPS_REPO:-"Bengo-Hub/devops-k8s"}
DEVOPS_DIR=${DEVOPS_DIR:-"$HOME/devops-k8s"}
VALUES_FILE_PATH=${VALUES_FILE_PATH:-"apps/${APP_NAME}/values.yaml"}

GIT_EMAIL=${GIT_EMAIL:-"dev@bengobox.com"}
GIT_USER=${GIT_USER:-"POS Bot"}
TRIVY_ECODE=${TRIVY_ECODE:-0}

if [[ -z ${GITHUB_SHA:-} ]]; then
  GIT_COMMIT_ID=$(git rev-parse --short=8 HEAD || echo "localbuild")
else
  GIT_COMMIT_ID=${GITHUB_SHA::8}
fi

info "Service : ${APP_NAME}"
info "Namespace: ${NAMESPACE}"
info "Image   : ${IMAGE_REPO}:${GIT_COMMIT_ID}"

for tool in git docker trivy; do
  command -v "$tool" >/dev/null || { error "$tool is required"; exit 1; }
done
if [[ ${DEPLOY} == "true" ]]; then
  for tool in kubectl helm yq jq; do
    command -v "$tool" >/dev/null || { error "$tool is required"; exit 1; }
  done
fi
success "Prerequisite checks passed"

info "Running Trivy filesystem scan"
trivy fs . --exit-code "$TRIVY_ECODE" --format table || true

info "Building Docker image"
# Build from workspace root to include shared/auth-client
# For local builds: docker build -f pos-service/Dockerfile -t pos-service:local .
# For CI builds: build from service directory, but Dockerfile expects workspace root context
if [[ -d "../shared/auth-client" ]]; then
  # We're in the service directory, build from parent (workspace root)
  DOCKER_BUILDKIT=1 docker build -f Dockerfile -t "${IMAGE_REPO}:${GIT_COMMIT_ID}" ..
else
  # We're already at workspace root or in CI
  DOCKER_BUILDKIT=1 docker build -f pos-service/Dockerfile -t "${IMAGE_REPO}:${GIT_COMMIT_ID}" .
fi
success "Docker build complete"

if [[ ${DEPLOY} != "true" ]]; then
  warn "DEPLOY=false -> skipping push/deploy"
  exit 0
fi

if [[ -n ${REGISTRY_USERNAME:-} && -n ${REGISTRY_PASSWORD:-} ]]; then
  echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY_SERVER" -u "$REGISTRY_USERNAME" --password-stdin
fi

docker push "${IMAGE_REPO}:${GIT_COMMIT_ID}"
success "Image pushed"

if [[ -n ${KUBE_CONFIG:-} ]]; then
  mkdir -p ~/.kube
  echo "$KUBE_CONFIG" | base64 -d > ~/.kube/config
  chmod 600 ~/.kube/config
  export KUBECONFIG=~/.kube/config
fi

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

if [[ -z ${CI:-}${GITHUB_ACTIONS:-} && -f KubeSecrets/devENV.yml ]]; then
  info "Applying local dev secrets"
  kubectl apply -n "$NAMESPACE" -f KubeSecrets/devENV.yml || warn "Failed to apply devENV.yml"
fi

if [[ -n ${REGISTRY_USERNAME:-} && -n ${REGISTRY_PASSWORD:-} ]]; then
  kubectl -n "$NAMESPACE" create secret docker-registry registry-credentials \
    --docker-server="$REGISTRY_SERVER" \
    --docker-username="$REGISTRY_USERNAME" \
    --docker-password="$REGISTRY_PASSWORD" \
    --dry-run=client -o yaml | kubectl apply -f - || warn "registry secret creation failed"
fi

# Create per-service database if SETUP_DATABASES is enabled
if [[ "$SETUP_DATABASES" == "true" && -n "${KUBE_CONFIG:-}" ]]; then
  # Wait for PostgreSQL to be ready in infra namespace
  if kubectl -n infra get statefulset postgresql >/dev/null 2>&1; then
    info "Waiting for PostgreSQL to be ready..."
    kubectl -n infra rollout status statefulset/postgresql --timeout=180s || warn "PostgreSQL not fully ready"
    
    # Create service database using devops-k8s script
    if [[ -d "$DEVOPS_DIR" ]] || [[ -n "${DEVOPS_REPO:-}" ]]; then
      # Ensure devops repo is cloned
      if [[ ! -d "$DEVOPS_DIR" ]]; then
        TOKEN="${GH_PAT:-${GIT_SECRET:-${GITHUB_TOKEN:-}}}"
        CLONE_URL="https://github.com/${DEVOPS_REPO}.git"
        [[ -n $TOKEN ]] && CLONE_URL="https://x-access-token:${TOKEN}@github.com/${DEVOPS_REPO}.git"
        git clone "$CLONE_URL" "$DEVOPS_DIR" || { warn "Unable to clone devops repo for database setup"; }
      fi
      
      if [[ -d "$DEVOPS_DIR" && -f "$DEVOPS_DIR/scripts/create-service-database.sh" ]]; then
        info "Creating database '${SERVICE_DB_NAME}' for service ${APP_NAME}..."
        SERVICE_DB_NAME="$SERVICE_DB_NAME" \
        APP_NAME="$APP_NAME" \
        NAMESPACE="$NAMESPACE" \
        bash "$DEVOPS_DIR/scripts/create-service-database.sh" || warn "Database creation failed or already exists"
      fi
    fi
  else
    warn "PostgreSQL not found in infra namespace - skipping database creation"
  fi
fi

if ! kubectl -n "$NAMESPACE" get secret "$ENV_SECRET_NAME" >/dev/null 2>&1; then
  warn "Secret $ENV_SECRET_NAME not found - creating placeholder"
  kubectl -n "$NAMESPACE" create secret generic "$ENV_SECRET_NAME" \
    --from-literal=POS_POSTGRES_URL="postgresql://${SERVICE_DB_USER}:PASSWORD@postgresql.infra.svc.cluster.local:5432/${SERVICE_DB_NAME}?sslmode=disable" \
    --from-literal=POS_REDIS_ADDR="redis-master.infra.svc.cluster.local:6379" \
    --from-literal=POS_NATS_URL="nats://nats.messaging.svc.cluster.local:4222" || true
fi

TOKEN="${GH_PAT:-${GIT_SECRET:-${GITHUB_TOKEN:-}}}"
CLONE_URL="https://github.com/${DEVOPS_REPO}.git"
[[ -n $TOKEN ]] && CLONE_URL="https://x-access-token:${TOKEN}@github.com/${DEVOPS_REPO}.git"

if [[ ! -d $DEVOPS_DIR ]]; then
  git clone "$CLONE_URL" "$DEVOPS_DIR" || { warn "Unable to clone devops repo"; DEVOPS_DIR=""; }
fi

if [[ -n $DEVOPS_DIR && -d $DEVOPS_DIR ]]; then
  pushd "$DEVOPS_DIR" >/dev/null || true
  git config user.email "$GIT_EMAIL"
  git config user.name "$GIT_USER"
  git fetch origin main || true
  git checkout main || git checkout -b main || true
  if [[ -f "$VALUES_FILE_PATH" ]]; then
    IMAGE_REPO_ENV="$IMAGE_REPO" IMAGE_TAG_ENV="$GIT_COMMIT_ID" \
      yq e -i '.image.repository = strenv(IMAGE_REPO_ENV) | .image.tag = strenv(IMAGE_TAG_ENV)' "$VALUES_FILE_PATH"
    git add "$VALUES_FILE_PATH"
    git commit -m "${APP_NAME}:${GIT_COMMIT_ID} released" || true
    [[ -n $TOKEN ]] && git push origin HEAD:main || warn "Skipped pushing values (no token)"
  else
    warn "${VALUES_FILE_PATH} not found in devops repo"
  fi
  popd >/dev/null || true
fi

info "Deployment summary"
echo "  Image      : ${IMAGE_REPO}:${GIT_COMMIT_ID}"
echo "  Namespace  : ${NAMESPACE}"
echo "  Databases  : ${SETUP_DATABASES} (${DB_TYPES})"

