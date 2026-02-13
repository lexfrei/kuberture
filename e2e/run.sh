#!/usr/bin/env bash
set -euo pipefail

# E2E test for kuberture using kind.
#
# Creates a kind cluster, builds and loads the kuberture image,
# installs the Helm chart, and verifies that the controller creates
# a headless Service with the expected annotations.
#
# Usage:
#   bash e2e/run.sh [--skip-build] [--keep-cluster]

CLUSTER_NAME="kuberture-e2e"
IMAGE_NAME="ghcr.io/lexfrei/kuberture"
IMAGE_TAG="e2e-test"
RELEASE_NAME="kuberture"
NAMESPACE="kuberture-e2e"
OUTPUT_SERVICE="kuberture-e2e-test"
OUTPUT_HOSTNAME="e2e.test.example.com"
ANNOTATION_PREFIX="external-dns.alpha.kubernetes.io"
EXPECTED_TTL="60"

SKIP_BUILD=false
KEEP_CLUSTER=false

# Detect Colima docker socket if DOCKER_HOST is not already set.
if [[ -z "${DOCKER_HOST:-}" ]]; then
    COLIMA_SOCK="${HOME}/.colima/default/docker.sock"
    if [[ -S "${COLIMA_SOCK}" ]]; then
        export DOCKER_HOST="unix://${COLIMA_SOCK}"
    fi
fi

for arg in "$@"; do
    case "${arg}" in
        --skip-build)  SKIP_BUILD=true ;;
        --keep-cluster) KEEP_CLUSTER=true ;;
        *) echo "Unknown argument: ${arg}"; exit 1 ;;
    esac
done

# --- Colours -----------------------------------------------------------

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

pass() { echo -e "  ${GREEN}PASS${NC} $1"; }
fail() { echo -e "  ${RED}FAIL${NC} $1"; FAILURES=$((FAILURES + 1)); }
info() { echo -e "${YELLOW}==>${NC} $1"; }

FAILURES=0

# --- Dependency checks --------------------------------------------------

check_deps() {
    local missing=()
    for cmd in kind kubectl helm docker jq; do
        if ! command -v "${cmd}" &>/dev/null; then
            missing+=("${cmd}")
        fi
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        echo "Missing required tools: ${missing[*]}"
        exit 1
    fi
}

# --- Cleanup ------------------------------------------------------------

cleanup() {
    if [[ "${KEEP_CLUSTER}" == "true" ]]; then
        info "Keeping cluster ${CLUSTER_NAME} (--keep-cluster)"
        info "Delete manually: kind delete cluster --name ${CLUSTER_NAME}"
        return
    fi
    info "Cleaning up kind cluster ${CLUSTER_NAME}"
    kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
}

trap cleanup EXIT

# --- Main ---------------------------------------------------------------

main() {
    check_deps

    info "Creating kind cluster ${CLUSTER_NAME}"
    kind create cluster --name "${CLUSTER_NAME}" --wait 60s

    export KUBECONFIG="/tmp/kuberture-e2e-kubeconfig"
    kind get kubeconfig --name "${CLUSTER_NAME}" > "${KUBECONFIG}"

    info "Waiting for cluster to be ready"
    kubectl --context "kind-${CLUSTER_NAME}" wait --for=condition=Ready nodes --all --timeout=120s

    if [[ "${SKIP_BUILD}" == "false" ]]; then
        info "Building container image ${IMAGE_NAME}:${IMAGE_TAG}"
        docker build --tag "${IMAGE_NAME}:${IMAGE_TAG}" --file Containerfile .
    fi

    info "Loading image into kind"
    kind load docker-image "${IMAGE_NAME}:${IMAGE_TAG}" --name "${CLUSTER_NAME}"

    info "Installing Helm chart"
    kubectl --context "kind-${CLUSTER_NAME}" create namespace "${NAMESPACE}" --dry-run=client --output=yaml | \
        kubectl --context "kind-${CLUSTER_NAME}" apply --filename=-

    if ! helm install "${RELEASE_NAME}" ./chart \
        --kube-context "kind-${CLUSTER_NAME}" \
        --namespace "${NAMESPACE}" \
        --set image.repository="${IMAGE_NAME}" \
        --set image.tag="${IMAGE_TAG}" \
        --set image.pullPolicy=Never \
        --set "config.outputs[0].name=e2e-test" \
        --set "config.outputs[0].hostname[0]=${OUTPUT_HOSTNAME}" \
        --set "config.outputs[0].serviceName=${OUTPUT_SERVICE}" \
        --set "config.outputs[0].recordTTL=60" \
        --wait \
        --timeout 180s; then
        info "Debug: Helm install failed, dumping diagnostics"
        kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
            get pods --output=wide 2>/dev/null || true
        kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
            describe deployment/"${RELEASE_NAME}" 2>/dev/null || true
        kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
            logs deployment/"${RELEASE_NAME}" --tail=100 2>/dev/null || true
        exit 1
    fi

    info "Waiting for deployment rollout"
    kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
        rollout status deployment/"${RELEASE_NAME}" --timeout=60s

    info "Waiting for Service to be created by controller"
    wait_for_service

    info "Running assertions"
    run_assertions

    echo ""
    if [[ ${FAILURES} -eq 0 ]]; then
        echo -e "${GREEN}All e2e tests passed.${NC}"
    else
        echo -e "${RED}${FAILURES} assertion(s) failed.${NC}"
        # Dump debug info on failure.
        info "Debug: controller logs"
        kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
            logs deployment/"${RELEASE_NAME}" --tail=50 2>/dev/null || true
        info "Debug: Service YAML"
        kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" get svc "${OUTPUT_SERVICE}" \
            --output=yaml 2>/dev/null || true
        exit 1
    fi
}

# --- Wait for Service ---------------------------------------------------

wait_for_service() {
    local retries=30
    local delay=2

    for i in $(seq 1 "${retries}"); do
        if kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" get svc "${OUTPUT_SERVICE}" &>/dev/null; then
            return 0
        fi
        echo "  Waiting for Service ${OUTPUT_SERVICE}... (${i}/${retries})"
        sleep "${delay}"
    done

    fail "Service ${OUTPUT_SERVICE} was not created within $((retries * delay))s"
    info "Debug: controller logs"
    kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
        logs deployment/"${RELEASE_NAME}" --tail=50 2>/dev/null || true
    exit 1
}

# --- Assertions ---------------------------------------------------------

run_assertions() {
    local svc_json
    svc_json="$(kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" get svc "${OUTPUT_SERVICE}" --output=json)"

    # 1. Service is headless (clusterIP == None).
    local cluster_ip
    cluster_ip="$(echo "${svc_json}" | jq -r '.spec.clusterIP')"
    if [[ "${cluster_ip}" == "None" ]]; then
        pass "Service is headless (clusterIP=None)"
    else
        fail "Service clusterIP = '${cluster_ip}', expected 'None'"
    fi

    # 2. hostname annotation matches config.
    local hostname_ann
    hostname_ann="$(echo "${svc_json}" | jq -r ".metadata.annotations[\"${ANNOTATION_PREFIX}/hostname\"]")"
    if [[ "${hostname_ann}" == "${OUTPUT_HOSTNAME}" ]]; then
        pass "hostname annotation = '${hostname_ann}'"
    else
        fail "hostname annotation = '${hostname_ann}', expected '${OUTPUT_HOSTNAME}'"
    fi

    # 3. target annotation is non-empty and contains IP addresses.
    local target_ann
    target_ann="$(echo "${svc_json}" | jq -r ".metadata.annotations[\"${ANNOTATION_PREFIX}/target\"]")"
    if [[ -z "${target_ann}" || "${target_ann}" == "null" ]]; then
        fail "target annotation is empty"
    else
        pass "target annotation is non-empty: '${target_ann}'"

        # Verify at least one entry looks like an IP address.
        if echo "${target_ann}" | grep --quiet --extended-regexp '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+'; then
            pass "target contains IPv4 address(es)"
        else
            fail "target '${target_ann}' does not contain a recognisable IPv4 address"
        fi
    fi

    # 4. ttl annotation matches config.
    local ttl_ann
    ttl_ann="$(echo "${svc_json}" | jq -r ".metadata.annotations[\"${ANNOTATION_PREFIX}/ttl\"]")"
    if [[ "${ttl_ann}" == "${EXPECTED_TTL}" ]]; then
        pass "ttl annotation = '${ttl_ann}'"
    else
        fail "ttl annotation = '${ttl_ann}', expected '${EXPECTED_TTL}'"
    fi

    # 5. Service type is ClusterIP.
    local svc_type
    svc_type="$(echo "${svc_json}" | jq -r '.spec.type')"
    if [[ "${svc_type}" == "ClusterIP" ]]; then
        pass "Service type = ClusterIP"
    else
        fail "Service type = '${svc_type}', expected 'ClusterIP'"
    fi

    # 6. Controller pod is ready (readiness probe passed).
    local ready_replicas
    ready_replicas="$(kubectl --context "kind-${CLUSTER_NAME}" --namespace "${NAMESPACE}" \
        get deployment "${RELEASE_NAME}" --output=jsonpath='{.status.readyReplicas}')"
    if [[ "${ready_replicas}" -ge 1 ]]; then
        pass "Controller has ${ready_replicas} ready replica(s)"
    else
        fail "Controller has ${ready_replicas} ready replicas, expected >= 1"
    fi

    # 7. Service has ownerReference pointing to the controller Deployment.
    local owner_kind
    owner_kind="$(echo "${svc_json}" | jq -r '.metadata.ownerReferences[0].kind // empty')"
    if [[ "${owner_kind}" == "Deployment" ]]; then
        pass "Service has ownerReference kind=Deployment"
    else
        fail "Service ownerReference kind = '${owner_kind}', expected 'Deployment'"
    fi

    local owner_name
    owner_name="$(echo "${svc_json}" | jq -r '.metadata.ownerReferences[0].name // empty')"
    if [[ "${owner_name}" == "${RELEASE_NAME}" ]]; then
        pass "Service ownerReference name = '${owner_name}'"
    else
        fail "Service ownerReference name = '${owner_name}', expected '${RELEASE_NAME}'"
    fi
}

main "$@"
