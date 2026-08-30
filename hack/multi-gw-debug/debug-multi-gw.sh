#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENV_DIR="${SCRIPT_DIR}/.venv"
REQUIREMENTS_FILE="${SCRIPT_DIR}/requirements.txt"

usage() {
    cat <<EOF
Usage: $(basename "$0") <mode> <context-1> <context-2>
       $(basename "$0") custom <command> <context-1> <context-2>

Modes:
  http-summary     Run tcpdump-http-summary.sh on each gateway pod
  traffic-amount   Run tcpdump-traffic-amount.sh on each gateway pod
  custom           Run <command> in every gateway pod terminal

Examples:
  $(basename "$0") http-summary kind-cl01 kind-cl02
  $(basename "$0") custom "tcpdump -i any port 443" kind-cl01 kind-cl02
EOF
}

if [ "$#" -ne 3 ] && [ "$#" -ne 4 ]; then
    usage >&2
    exit 1
fi

MODE="$1"

if [ "${MODE}" = "custom" ]; then
    if [ "$#" -ne 4 ]; then
        usage >&2
        exit 1
    fi
    CUSTOM_COMMAND="$2"
    if [ -z "${CUSTOM_COMMAND}" ]; then
        echo "Custom command cannot be empty." >&2
        usage >&2
        exit 1
    fi
    CTX1="$3"
    CTX2="$4"
    TARGET="${CUSTOM_COMMAND}"
else
    if [ "$#" -ne 3 ]; then
        usage >&2
        exit 1
    fi
    CTX1="$2"
    CTX2="$3"

    case "${MODE}" in
        http-summary)
            TARGET="${SCRIPT_DIR}/tcpdump-http-summary.sh"
            ;;
        traffic-amount)
            TARGET="${SCRIPT_DIR}/tcpdump-traffic-amount.sh"
            ;;
        *)
            echo "Unknown mode: ${MODE}" >&2
            usage >&2
            exit 1
            ;;
    esac

    if [ ! -f "${TARGET}" ]; then
        echo "Target script not found: ${TARGET}" >&2
        exit 1
    fi
fi

for cmd in kubectl python3; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        echo "Required command not found: ${cmd}" >&2
        exit 1
    fi
done

if [ ! -d "${VENV_DIR}" ]; then
    echo "Creating Python virtual environment in ${VENV_DIR}..."
    python3 -m venv "${VENV_DIR}"
fi

if [ ! -f "${REQUIREMENTS_FILE}" ]; then
    echo "Requirements file not found: ${REQUIREMENTS_FILE}" >&2
    exit 1
fi

echo "Installing Python dependencies in virtual environment..."
"${VENV_DIR}/bin/pip" install -q -r "${REQUIREMENTS_FILE}"

get_cluster_id() {
    local ctx="$1"
    kubectl --context "${ctx}" get configmap liqo-clusterid-configmap -n castai-omni \
        -o jsonpath='{.data.CLUSTER_ID}' 2>/dev/null || true
}

get_gateway_pods() {
    local ctx="$1"
    kubectl --context "${ctx}" get pods -A \
        -l networking.liqo.io/component=gateway \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null || true
}

filter_pods_by_remote_cluster() {
    local ctx="$1"
    local remote_cluster_id="$2"
    local pods="$3"
    local pod ns name

    if [ -z "${remote_cluster_id}" ]; then
        echo "${pods}"
        return
    fi

    while IFS= read -r pod; do
        [ -z "${pod}" ] && continue
        ns="${pod%/*}"
        name="${pod#*/}"
        if kubectl --context "${ctx}" -n "${ns}" get pod "${name}" -o yaml 2>/dev/null | \
                grep -q -- "--remote-cluster-id=${remote_cluster_id}"; then
            echo "${pod}"
        fi
    done <<< "${pods}"
}

CLUSTER_ID1="$(get_cluster_id "${CTX1}")"
CLUSTER_ID2="$(get_cluster_id "${CTX2}")"

if [ -z "${CLUSTER_ID1}" ]; then
    echo "Warning: could not read cluster ID for context ${CTX1} from configmap castai-omni/liqo-clusterid-configmap" >&2
fi
if [ -z "${CLUSTER_ID2}" ]; then
    echo "Warning: could not read cluster ID for context ${CTX2} from configmap castai-omni/liqo-clusterid-configmap" >&2
fi

PODS1_RAW="$(get_gateway_pods "${CTX1}")"
PODS2_RAW="$(get_gateway_pods "${CTX2}")"

PODS1_RAW="$(filter_pods_by_remote_cluster "${CTX1}" "${CLUSTER_ID2}" "${PODS1_RAW}")"
PODS2_RAW="$(filter_pods_by_remote_cluster "${CTX2}" "${CLUSTER_ID1}" "${PODS2_RAW}")"

if [ -z "${PODS1_RAW}" ] && [ -z "${PODS2_RAW}" ]; then
    echo "No gateway pods found for the cross-cluster pair (label: networking.liqo.io/component=gateway, remote-cluster-id points to the other context)." >&2
    exit 1
fi

PODS1_LIST="$(echo "${PODS1_RAW}" | grep -v '^$' | paste -sd ',' -)"
PODS2_LIST="$(echo "${PODS2_RAW}" | grep -v '^$' | paste -sd ',' -)"

echo "Context ${CTX1} (${CLUSTER_ID1:-unknown}): $(echo "${PODS1_RAW}" | grep -c '^.') matching gateway pod(s)"
echo "Context ${CTX2} (${CLUSTER_ID2:-unknown}): $(echo "${PODS2_RAW}" | grep -c '^.') matching gateway pod(s)"
echo "Launching iTerm2 layout..."

exec "${VENV_DIR}/bin/python3" "${SCRIPT_DIR}/iterm-layout.py" \
    "${MODE}" \
    "${CTX1}" \
    "${CTX2}" \
    "${PODS1_LIST}" \
    "${PODS2_LIST}" \
    "${TARGET}"
