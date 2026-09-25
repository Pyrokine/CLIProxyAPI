#!/usr/bin/env bash

set -euo pipefail

PANEL_REPO="${PANEL_REPO:-Pyrokine/Cli-Proxy-API-Management-Center}"
PANEL_TAG="${PANEL_TAG:-v1.23.1-aug.1}"
PANEL_OUTPUT="${PANEL_OUTPUT:-static/management.html}"
PANEL_SOURCE="${PANEL_SOURCE:-}"

output_dir="$(dirname "${PANEL_OUTPUT}")"
mkdir -p "${output_dir}"

release_json="$(mktemp)"
temp_asset="$(mktemp "${output_dir}/.management.XXXXXX")"
cleanup() {
  rm -f "${release_json}" "${temp_asset}"
}
trap cleanup EXIT

source_description=""
if [[ -n "${PANEL_SOURCE}" ]]; then
  if [[ ! -f "${PANEL_SOURCE}" ]]; then
    echo "Error: management panel source not found: ${PANEL_SOURCE}" >&2
    exit 1
  fi
  cp "${PANEL_SOURCE}" "${temp_asset}"
  source_description="${PANEL_SOURCE}"
else
  auth_header=()
  if [[ -n "${GH_TOKEN:-}" ]]; then
    auth_header=(-H "Authorization: Bearer ${GH_TOKEN}")
  fi

  curl -fsSL \
    -H 'Accept: application/vnd.github+json' \
    -H 'User-Agent: cli-proxy-api-docker-build' \
    "${auth_header[@]}" \
    "https://api.github.com/repos/${PANEL_REPO}/releases/tags/${PANEL_TAG}" \
    > "${release_json}"

  asset_url="$(
    python3 - "${release_json}" "${PANEL_TAG}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    release = json.load(fh)

expected_tag = sys.argv[2]
actual_tag = release.get("tag_name")
if actual_tag != expected_tag:
    raise SystemExit(f"management panel tag mismatch: expected {expected_tag}, got {actual_tag!r}")

for asset in release.get("assets", []):
    if asset.get("name") == "management.html" and asset.get("browser_download_url"):
        print(asset["browser_download_url"])
        break
else:
    raise SystemExit(f"management.html release asset not found for {expected_tag}")
PY
  )"

  curl -fsSL -o "${temp_asset}" "${asset_url}"
  source_description="${PANEL_REPO}@${PANEL_TAG}"
fi

test -s "${temp_asset}"
if grep -q 'Management panel asset is not included in this Docker image' "${temp_asset}"; then
  echo "Error: downloaded management panel is the fallback page" >&2
  exit 1
fi

mv -f "${temp_asset}" "${PANEL_OUTPUT}"
echo "Prepared ${PANEL_OUTPUT} from ${source_description}"
