#!/usr/bin/env bash
# Run the dependency-free pull-request test subset and reject silent collapse.
set -uo pipefail

# A fresh isolated run on 2026-08-19 reported 182 passing tests, 25 skipped,
# and 0 failed, including subtests. This floor leaves 32 passing tests of
# removal headroom while still catching a materially emptied subset.
min_hermetic_tests=150

log="$(mktemp)" || {
  echo "::error::could not create a temporary test log"
  exit 1
}
if [[ -z "$log" || ! -f "$log" ]]; then
  echo "::error::temporary test log was not created"
  exit 1
fi

cleanup() {
  if [[ -n "${log:-}" && -f "$log" ]]; then
    rm -f -- "$log"
  fi
}
trap cleanup EXIT

echo "+ GOWORK=off go test -race -short -count=1 -v ./..."
GOWORK=off go test -race -short -count=1 -v ./... 2>&1 | tee "$log"
pipeline_status=("${PIPESTATUS[@]}")
go_test_exit="${pipeline_status[0]}"
tee_exit="${pipeline_status[1]}"

pass_count="$(grep -cE '^[[:space:]]*--- PASS:' "$log" || true)"
fail_count="$(grep -cE '^[[:space:]]*--- FAIL:' "$log" || true)"
skip_count="$(grep -cE '^[[:space:]]*--- SKIP:' "$log" || true)"

echo
echo "hermetic subset summary: ${pass_count} passed, ${skip_count} skipped, ${fail_count} failed (go test exit ${go_test_exit})"

if [[ "$tee_exit" -ne 0 ]]; then
  echo "::error::could not capture the complete go test log (tee exit ${tee_exit})"
  exit 1
fi

if [[ "$go_test_exit" -ne 0 || "$fail_count" -ne 0 ]]; then
  echo "::error::go test exited ${go_test_exit} with ${fail_count} failing test(s)"
  exit 1
fi

if [[ "$pass_count" -lt "$min_hermetic_tests" ]]; then
  echo "::error::only ${pass_count} hermetic tests passed (want >= ${min_hermetic_tests}); the subset may have silently collapsed"
  exit 1
fi

echo "hermetic gate satisfied: ${pass_count} >= ${min_hermetic_tests} (0 failed, ${skip_count} skipped)"
