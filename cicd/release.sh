#!/usr/bin/env bash

# Publish the four Seek archives, their checksums, and the SBOM. --clobber makes
# a repeated upload replace partial release assets.
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
dist_dir=${DIST_DIR:-${repo_root}/dist}
release_tag=${RELEASE_TAG:-}
sbom_file=${SBOM_FILE:-${repo_root}/sbom.cyclonedx.json}

fail() {
	echo "release: $*" >&2
	exit 1
}

[[ -n ${release_tag} ]] || fail "RELEASE_TAG is required"
command -v gh >/dev/null 2>&1 || fail "GitHub CLI is required"

archive_names=(
	seek_darwin_amd64.tar.gz
	seek_darwin_arm64.tar.gz
	seek_linux_amd64.tar.gz
	seek_linux_arm64.tar.gz
)
archives=()
temporary_checksums="${dist_dir}/checksums.txt.tmp.$$"
trap 'rm -f "${temporary_checksums}"' EXIT
: > "${temporary_checksums}"

for archive_name in "${archive_names[@]}"; do
	archive_file="${dist_dir}/${archive_name}"
	[[ -f ${archive_file} ]] || fail "missing archive: ${archive_file}"
	archives+=("${archive_file}")
	if command -v sha256sum >/dev/null 2>&1; then
		digest=$(sha256sum "${archive_file}" | awk '{print $1}')
	else
		digest=$(shasum -a 256 "${archive_file}" | awk '{print $1}')
	fi
	printf '%s  %s\n' "${digest}" "${archive_name}" >> "${temporary_checksums}"
done
mv -f "${temporary_checksums}" "${dist_dir}/checksums.txt"

[[ -f ${sbom_file} ]] || fail "SBOM does not exist: ${sbom_file}"
upload_files=("${archives[@]}" "${dist_dir}/checksums.txt" "${sbom_file}")

gh release view "${release_tag}" >/dev/null || fail "release does not exist: ${release_tag}"
gh release upload "${release_tag}" "${upload_files[@]}" --clobber
