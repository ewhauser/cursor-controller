#!/usr/bin/env bash
set -euo pipefail
kind delete cluster --name "${CLUSTER:-cursor-e2e}"
