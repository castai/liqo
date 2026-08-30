#!/usr/bin/env bash

set -e
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

cd "${REPO_ROOT}"

# Environment variables respected by build/liqo/build.sh
DOCKER_REGISTRY="${DOCKER_REGISTRY:-us-docker.pkg.dev}"
DOCKER_ORGANIZATION="${DOCKER_ORGANIZATION:-castai-hub/library/liqo}"
DOCKER_TAG="${DOCKER_TAG:-$(git rev-parse HEAD)}"
DOCKER_PUSH="${DOCKER_PUSH:-false}"
ARCHS="${ARCHS:-linux/$(go env GOARCH)}"

export DOCKER_REGISTRY DOCKER_ORGANIZATION DOCKER_TAG DOCKER_PUSH ARCHS

# Go components handled by build/liqo/build.sh
GO_COMPONENTS=(
    crd-replicator
    ipam
    liqo-controller-manager
    webhook
    uninstaller
    virtual-kubelet
    metric-agent
    telemetry
    proxy
    gateway
    gateway/wireguard
    gateway/geneve
    fabric
)

# Non-Go components built directly from their Dockerfiles
STANDARD_COMPONENTS=(
    cert-creator
    liqo-crd-upgrade
)

image_component_for() {
    local component_path="$1"
    local basename_component
    basename_component=$(basename "${component_path}")

    if [[ "${basename_component}" == "geneve" || "${basename_component}" == "wireguard" ]]; then
        echo "gateway/${basename_component}"
    else
        echo "${basename_component}"
    fi
}

image_tag_for() {
    local image_component="$1"
    local tag

    if [[ "${DOCKER_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
        tag="${DOCKER_REGISTRY}/${DOCKER_ORGANIZATION}/${image_component}:${DOCKER_TAG}"
    else
        tag="${DOCKER_REGISTRY}/${DOCKER_ORGANIZATION}/${image_component}-ci:${DOCKER_TAG}"
    fi

    echo "${tag}"
}

load_into_kind() {
    local image_tag="$1"
    local clusters

    clusters=$(kind get clusters 2>/dev/null || true)
    if [ -z "${clusters}" ]; then
        echo "No kind clusters found; image ${image_tag} was built locally only."
        return
    fi

    for cluster in ${clusters}; do
        echo "Loading image ${image_tag} into kind cluster '${cluster}'..."
        kind load docker-image "${image_tag}" --name "${cluster}"
    done
}

build_go_component() {
    local component="$1"
    local component_path="./cmd/${component}"
    local image_component
    image_component=$(image_component_for "${component_path}")
    local image_tag
    image_tag=$(image_tag_for "${image_component}")

    echo "============================================"
    echo "Building Go component: ${component}"
    echo "============================================"

    "${REPO_ROOT}/build/liqo/build.sh" "${component_path}"

    load_into_kind "${image_tag}"
}

build_standard_component() {
    local component="$1"
    local dockerfile_path="build/${component}/Dockerfile"
    local image_tag
    image_tag=$(image_tag_for "${component}")

    echo "============================================"
    echo "Building standard component: ${component}"
    echo "============================================"

    local platform
    platform="linux/$(go env GOARCH)"

    docker buildx build --platform "${platform}" \
        --build-arg COMPONENT="${component}" \
        -t "${image_tag}" \
        -f "./${dockerfile_path}" . \
        --load

    load_into_kind "${image_tag}"
}

echo "Building all Liqo images and loading them into kind clusters..."
echo "DOCKER_REGISTRY=${DOCKER_REGISTRY}"
echo "DOCKER_ORGANIZATION=${DOCKER_ORGANIZATION}"
echo "DOCKER_TAG=${DOCKER_TAG}"
echo "DOCKER_PUSH=${DOCKER_PUSH}"
echo "ARCHS=${ARCHS}"

for component in "${GO_COMPONENTS[@]}"; do
    build_go_component "${component}"
done

for component in "${STANDARD_COMPONENTS[@]}"; do
    build_standard_component "${component}"
done

echo "All images built and loaded into kind clusters."
