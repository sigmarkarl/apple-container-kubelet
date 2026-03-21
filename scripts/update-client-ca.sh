#!/usr/bin/env bash
# Extracts the cluster CA certificate from your current kubeconfig context
# and updates the applevm-kubelet config to use it for mTLS.
#
# Usage:
#   ./scripts/update-client-ca.sh [config-path]
#
# Arguments:
#   config-path  Path to applevm-kubelet config.toml
#               (default: /etc/applevm-kubelet/config.toml)
#
# When you switch Kubernetes clusters, run this script to update the CA cert
# so the kubelet server only accepts connections from the new cluster's API server.

set -euo pipefail

CONFIG_PATH="${1:-/etc/applevm-kubelet/config.toml}"
CA_DIR="$(dirname "$CONFIG_PATH")"
CA_PATH="${CA_DIR}/cluster-ca.crt"

echo "Extracting CA cert from current kubeconfig context..."

CONTEXT="$(kubectl config current-context)"
CLUSTER="$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"${CONTEXT}\")].context.cluster}")"
CA_DATA="$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name==\"${CLUSTER}\")].cluster.certificate-authority-data}")"

if [ -z "$CA_DATA" ]; then
    echo "Error: No certificate-authority-data found for cluster '${CLUSTER}'." >&2
    echo "The cluster may use a file-based CA reference instead of inline data." >&2
    exit 1
fi

mkdir -p "$CA_DIR"
echo "$CA_DATA" | base64 -d > "$CA_PATH"
echo "Wrote CA cert to ${CA_PATH}"

# Verify it's a valid PEM certificate
if ! openssl x509 -in "$CA_PATH" -noout -subject 2>/dev/null; then
    echo "Warning: ${CA_PATH} does not appear to be a valid certificate." >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEFAULT_TOML="${SCRIPT_DIR}/../config/default.toml"

if [ ! -f "$CONFIG_PATH" ]; then
    if [ -f "$DEFAULT_TOML" ]; then
        cp "$DEFAULT_TOML" "$CONFIG_PATH"
        echo "Created config from default template at ${CONFIG_PATH}"
    else
        echo "Warning: No existing config and no default template found. Creating minimal config." >&2
        printf '[tls]\nclient_ca_path = ""\n' > "$CONFIG_PATH"
    fi
fi

# Update or insert client_ca_path in the config
if grep -q 'client_ca_path' "$CONFIG_PATH" 2>/dev/null; then
    sed -i.bak "s|client_ca_path.*=.*|client_ca_path = \"${CA_PATH}\"|" "$CONFIG_PATH"
    rm -f "${CONFIG_PATH}.bak"
else
    printf '\n[tls]\nclient_ca_path = "%s"\n' "$CA_PATH" >> "$CONFIG_PATH"
fi
echo "Updated ${CONFIG_PATH} with client_ca_path = \"${CA_PATH}\""

echo ""
echo "Cluster:  ${CLUSTER}"
echo "Context:  ${CONTEXT}"
echo "CA cert:  ${CA_PATH}"
echo "Config:   ${CONFIG_PATH}"
echo ""
echo "Restart applevm-kubelet to pick up the new CA."
