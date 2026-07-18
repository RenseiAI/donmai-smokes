#!/usr/bin/env bash
# run.sh — gated kit-toolchain cloud (e2b) provisioning smoke.
#
# KIT_TOOLCHAIN_KIT selects the fixture/manifest pair (default: ts-next; also
# swift). KIT_E2B_DRY_RUN=1 validates and prints the exact command plan without
# loading credentials, installing the e2b SDK, or creating a sandbox.
#
# Real mode is mechanism-level proof: the selected kit manifest is detected,
# composed, and provisioned after a deterministic fixture is staged inside one
# e2b sandbox. It does not drive the daemon session path or signature trust.
#
# GATING: real mode requires E2B_API_KEY. Without it (normal CI) this exits 0 as
# an explicit skip, so the smoke never burns credits unintentionally.
#
# Env:
#   KIT_TOOLCHAIN_KIT  ts-next (default) or swift.
#   KIT_E2B_DRY_RUN    1 = validate/print the plan with no cloud access.
#   KIT_E2B_LOAD_ENV   0 = do not source ../.env.local (safe skip testing).
#   KIT_E2B_TIMEOUT    sandbox lifetime: 1-900 seconds (default by profile).
#   KIT_E2B_COMMAND_TIMEOUT command timeout: 1-600 seconds and no greater
#                           than KIT_E2B_TIMEOUT (default by profile).
#   E2B_API_KEY        required only for a real run.
#   TS_FIXTURE_REPO    optional git URL for the legacy ts-next path only;
#                      Swift always uploads its dependency-free local fixture.

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIT="${KIT_TOOLCHAIN_KIT:-ts-next}"

if [[ "${KIT_E2B_DRY_RUN:-0}" == "1" ]]; then
  exec python3 "${DIR}/kit_toolchain_e2b.py" --dry-run --kit "${KIT}"
fi

# Local operator convenience only. Dry-run intentionally returns before this
# block so command-plan validation can never load a cloud credential.
ENV_FILE="${DIR}/../.env.local"
if [[ "${KIT_E2B_LOAD_ENV:-1}" != "0" && -f "${ENV_FILE}" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
fi

if [[ -z "${E2B_API_KEY:-}" ]]; then
  echo "[skip] E2B_API_KEY not set — skipping kit-toolchain e2b smoke (gated)."
  exit 0
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "[error] python3 required" >&2
  exit 2
fi

# Use an isolated venv. The e2b 2.x SDK + protobuf 6 uses the pure-Python
# protobuf implementation here to avoid generated-stub incompatibilities.
VENV="${KIT_E2B_VENV:-${DIR}/.venv}"
if [[ ! -x "${VENV}/bin/python" ]]; then
  echo "[info] creating isolated kit smoke environment"
  python3 -m venv "${VENV}"
  "${VENV}/bin/pip" install --quiet --upgrade pip
  "${VENV}/bin/pip" install --quiet 'e2b>=2,<3' 'protobuf>=6,<7' tomli
fi

export PROTOCOL_BUFFERS_PYTHON_IMPLEMENTATION=python
exec "${VENV}/bin/python" "${DIR}/kit_toolchain_e2b.py" --kit "${KIT}"
