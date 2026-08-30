# Multi-Gateway Debugging Layout

This directory contains a helper that opens an iTerm2 split-pane view and runs
one of the existing tcpdump-based debug scripts on every Liqo gateway pod in two
Kubernetes contexts at once.

## Requirements

- **iTerm2** (macOS terminal emulator) with the Python API enabled.
- **Python 3** with the `venv` module available.
- **`kubectl`** configured with access to both clusters/contexts.
- Gateway pods must be labelled with `networking.liqo.io/component: gateway`.

### Enable the iTerm2 Python API

1. Open iTerm2.
2. Go to **Preferences** → **General** → **Magic**.
3. Check **Enable Python API**.
4. Restart iTerm2 if prompted.

## Files

- `debug-multi-gw.sh` — main entrypoint. Creates a local Python virtual
  environment and installs dependencies automatically.
- `iterm-layout.py` — iTerm2 Python API helper that creates the split panes.
- `tcpdump-http-summary.sh` — captures and summarizes HTTP traffic on gateway
  pods.
- `tcpdump-traffic-amount.sh` — counts HTTP packets on gateway pods.
- `requirements.txt` — Python dependencies (currently `iterm2`).

## Usage

Run the script from a shell inside iTerm2:

```bash
./hack/multi-gw-debug/debug-multi-gw.sh <mode> <context-1> <context-2>
```

On first run the script creates a Python virtual environment under
`hack/multi-gw-debug/.venv` and installs the dependencies listed in
`requirements.txt`. Subsequent runs reuse the existing virtual environment.

Supported modes:

- `http-summary` — runs `tcpdump-http-summary.sh` on each gateway pod.
- `traffic-amount` — runs `tcpdump-traffic-amount.sh` on each gateway pod.

Example:

```bash
./hack/multi-gw-debug/debug-multi-gw.sh http-summary kind-cl01 kind-cl02
```

## What it does

1. Reads the local cluster ID from each context by inspecting the
   `liqo-clusterid-configmap` ConfigMap in the `liqo` namespace.
2. Queries both Kubernetes contexts for pods labelled
   `networking.liqo.io/component: gateway`, then keeps only the gateway pods
   whose Pod spec contains `--remote-cluster-id=<ID_OF_THE_OTHER_CONTEXT>`.
   This ensures the left column shows only the gateways that peer with
   `<context-2>`, and the right column shows only the gateways that peer with
   `<context-1>`.
3. Uses the iTerm2 Python API to split the current terminal window:
   - **Left column**: gateway pods of `<context-1>` that peer with `<context-2>`.
   - **Right column**: gateway pods of `<context-2>` that peer with `<context-1>`.
   - Pods are aligned by row so you can easily compare the same logical row
     across the two clusters. If the clusters have a different number of
     matching gateway pods, the extra pods occupy additional rows in their
     column.
4. In each sub-terminal it copies the selected debug script into the pod and
   executes it interactively, clearing the terminal first.

## Notes

- The script must be launched from within an iTerm2 window so the Python API
  can connect to it.
- `kubectl exec -it` is used, so each pane behaves like an interactive shell
  session inside the gateway pod.
- The copied script is placed under `/tmp/` inside the pod and executed with
  `/bin/sh`, so it does not rely on the executable bit being preserved by
  `kubectl cp`.
