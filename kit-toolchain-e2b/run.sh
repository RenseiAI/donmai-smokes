#!/usr/bin/env bash
# run.sh — kit-toolchain cloud (e2b) provisioning smoke.
#
# Proves the donmai "kits pivot": inside a real CLOUD (e2b) sandbox, after a
# TS repo is staged, the kit's toolchain_install (node) runs AFTER the repo is
# present, then post_acquire (npm ci) + build/test. Asserts node is installed
# and `npm run build` / `npm test` succeed.
#
# LEVEL OF PROOF: mechanism-level. Drives donmai's kit manifest SCHEMA and the
# detect -> compose -> provision ORDERING semantics (kit_detect.go,
# compose.go, provisioner.go, runner/loop.go) against a real e2b sandbox via
# the e2b SDK. It does NOT drive the daemon session path — donmai ships no
# cloud-sandbox Execer yet (kit_execer.go: cloud Execer is "K2" future work; no
# provider/e2b package exists). See kit_toolchain_e2b.py header for the full
# rationale.
#
# GATING: requires E2B_API_KEY. Without it (normal CI) this SKIPS with exit 0,
# so it never runs unintentionally and never burns e2b credits.
#
# Env:
#   E2B_API_KEY      required to actually run (sourced from ../.env.local).
#   TS_FIXTURE_REPO  optional git URL to clone; if unset/unreachable the local
#                    fixtures/ts-next is uploaded (deterministic).

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load ../.env.local (repo root) if present.
ENV_FILE="${DIR}/../.env.local"
if [[ -f "${ENV_FILE}" ]]; then
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

# Use an ISOLATED venv (not --user site) — the e2b SDK pulls protobuf and a
# polluted shared site-packages reliably breaks it. The e2b 2.x SDK + protobuf
# 6 only works under the PURE-PYTHON protobuf impl (the C++ impl's
# FieldDescriptor lacks `is_repeated`, which e2b's generated stubs call), so we
# pin that via PROTOCOL_BUFFERS_PYTHON_IMPLEMENTATION. Verified 2026-05-28.
VENV="${KIT_E2B_VENV:-${DIR}/.venv}"
if [[ ! -x "${VENV}/bin/python" ]]; then
  echo "[info] creating isolated venv at ${VENV}…"
  python3 -m venv "${VENV}"
  "${VENV}/bin/pip" install --quiet --upgrade pip
  "${VENV}/bin/pip" install --quiet 'e2b>=2,<3' 'protobuf>=6,<7' tomli
fi

export PROTOCOL_BUFFERS_PYTHON_IMPLEMENTATION=python
exec "${VENV}/bin/python" "${DIR}/kit_toolchain_e2b.py"
