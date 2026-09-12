#!/usr/bin/env bash

# Shared helpers and versions for the maintainer-only search asset commands.

asset_script_name=$(basename "$0")

search_assets_ort_version() {
	echo "1.30.0"
}

fail() {
	echo "${asset_script_name}: $*" >&2
	exit 1
}

if command -v shasum >/dev/null 2>&1; then
	hash_command=shasum
elif command -v sha256sum >/dev/null 2>&1; then
	hash_command=sha256sum
else
	fail "missing command: shasum or sha256sum"
fi

file_sha256() {
	if [[ ${hash_command} == shasum ]]; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		sha256sum "$1" | awk '{print $1}'
	fi
}

show_metadata() {
	local bytes
	bytes=$(wc -c < "$2" | awk '{print $1}')
	echo "$1: ${bytes} bytes; SHA-256: $(file_sha256 "$2")"
}

download() {
	local label=$1
	local url=$2
	local destination=$3
	local partial="${destination}.part"

	if [[ -f ${destination} ]]; then
		echo "Using cached ${label}: ${destination}"
		show_metadata "${label}" "${destination}"
		return
	fi
	echo "Downloading ${label}: ${url}"
	if ! curl --fail --location --silent --show-error --continue-at - \
		--connect-timeout 30 --retry 5 --retry-all-errors --retry-delay 1 \
		--output "${partial}" "${url}"; then
		fail "download failed; run this command again to resume ${partial}"
	fi
	mv -f "${partial}" "${destination}"
	chmod 0644 "${destination}"
	show_metadata "${label}" "${destination}"
}
