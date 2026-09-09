#!/usr/bin/env bash

# This manual maintainer command updates tracked re-ranker resources. Product
# builds consume those resources and never invoke this script or fetch them.
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cache_root=${RERANK_ASSET_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/seek/rerank-assets-upgrade}
source_dir="${cache_root}/sources"
work_dir="${cache_root}/work"

revision=4bcdf5ed93f791259eb130b577a240f753d68dd8
model_url="https://huggingface.co/lightonai/LateOn-Code-edge/resolve/${revision}/model_int8.onnx"
tokenizer_url="https://huggingface.co/lightonai/LateOn-Code-edge/resolve/${revision}/tokenizer.json"

ort_version=1.29.0
ort_darwin_arm64_url="https://github.com/microsoft/onnxruntime/releases/download/v${ort_version}/onnxruntime-osx-arm64-${ort_version}.tgz"
ort_linux_amd64_url="https://github.com/microsoft/onnxruntime/releases/download/v${ort_version}/onnxruntime-linux-x64-${ort_version}.tgz"
ort_linux_arm64_url="https://github.com/microsoft/onnxruntime/releases/download/v${ort_version}/onnxruntime-linux-aarch64-${ort_version}.tgz"

fail() {
	echo "rerank-assets-upgrade: $*" >&2
	exit 1
}

for command_name in awk cmp curl tar wc zstd; do
	command -v "${command_name}" >/dev/null 2>&1 || fail "missing command: ${command_name}"
done
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

replace_if_changed() {
	local label=$1
	local temporary=$2
	local destination=$3
	local mode=$4

	if [[ -f ${destination} ]] && cmp -s "${temporary}" "${destination}"; then
		rm -f "${temporary}"
		echo "${label} is current: ${destination}"
	else
		mv -f "${temporary}" "${destination}"
		echo "Updated ${label}: ${destination}"
	fi
	chmod "${mode}" "${destination}"
	show_metadata "${label}" "${destination}"
}

package_asset() {
	local label=$1
	local source=$2
	local destination=$3
	local temporary="${destination}.tmp.$$"

	zstd -q -f -19 -T1 "${source}" -o "${temporary}"
	replace_if_changed "${label}" "${temporary}" "${destination}" 0644
}

extract_runtime() {
	local label=$1
	local archive=$2
	local member=$3
	local destination=$4
	local temporary="${destination}.tmp.$$"

	tar -xOf "${archive}" "${member}" > "${temporary}"
	replace_if_changed "${label}" "${temporary}" "${destination}" 0755
}

write_runtime_manifest() {
	local target=$1
	local file_name=$2
	local source=$3
	local destination=$4
	local bytes
	local sha256
	local temporary="${destination}.tmp.$$"

	bytes=$(wc -c < "${source}" | awk '{print $1}')
	sha256=$(file_sha256 "${source}")
	printf '{\n  "target": "%s",\n  "file_name": "%s",\n  "version": "%s",\n  "bytes": %s,\n  "sha256": "%s"\n}\n' \
		"${target}" "${file_name}" "${ort_version}" "${bytes}" "${sha256}" > "${temporary}"
	replace_if_changed "tracked ONNX Runtime ${target} manifest" \
		"${temporary}" "${destination}" 0644
}

mkdir -p "${source_dir}" "${work_dir}"

"${repo_root}/cicd/rerank-ort-darwin-amd64.sh"

model_source="${source_dir}/LateOn-Code-edge-${revision}-model_int8.onnx"
tokenizer_source="${source_dir}/LateOn-Code-edge-${revision}-tokenizer.json"
ort_darwin_amd64_source="${cache_root}/internal/onnxruntime-osx-x64-${ort_version}/libonnxruntime.${ort_version}.dylib"
ort_darwin_arm64_archive="${source_dir}/onnxruntime-osx-arm64-${ort_version}.tgz"
ort_linux_amd64_archive="${source_dir}/onnxruntime-linux-x64-${ort_version}.tgz"
ort_linux_arm64_archive="${source_dir}/onnxruntime-linux-aarch64-${ort_version}.tgz"
ort_darwin_arm64_source="${work_dir}/libonnxruntime-darwin-arm64-${ort_version}.dylib"
ort_linux_amd64_source="${work_dir}/libonnxruntime-linux-amd64-${ort_version}.so"
ort_linux_arm64_source="${work_dir}/libonnxruntime-linux-arm64-${ort_version}.so"

download "LateOn model" "${model_url}" "${model_source}"
download "LateOn tokenizer" "${tokenizer_url}" "${tokenizer_source}"
download "ONNX Runtime darwin-arm64 archive" "${ort_darwin_arm64_url}" \
	"${ort_darwin_arm64_archive}"
download "ONNX Runtime linux-amd64 archive" "${ort_linux_amd64_url}" \
	"${ort_linux_amd64_archive}"
download "ONNX Runtime linux-arm64 archive" "${ort_linux_arm64_url}" \
	"${ort_linux_arm64_archive}"

extract_runtime "ONNX Runtime darwin-arm64 source" \
	"${ort_darwin_arm64_archive}" \
	"./onnxruntime-osx-arm64-${ort_version}/lib/libonnxruntime.${ort_version}.dylib" \
	"${ort_darwin_arm64_source}"
extract_runtime "ONNX Runtime linux-amd64 source" \
	"${ort_linux_amd64_archive}" \
	"onnxruntime-linux-x64-${ort_version}/lib/libonnxruntime.so.${ort_version}" \
	"${ort_linux_amd64_source}"
extract_runtime "ONNX Runtime linux-arm64 source" \
	"${ort_linux_arm64_archive}" \
	"onnxruntime-linux-aarch64-${ort_version}/lib/libonnxruntime.so.${ort_version}" \
	"${ort_linux_arm64_source}"

mkdir -p \
	"${repo_root}/cmd/seek/rerank_assets" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_amd64" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_arm64" \
	"${repo_root}/cmd/seek/rerank_assets_linux_amd64" \
	"${repo_root}/cmd/seek/rerank_assets_linux_arm64"
package_asset "tracked LateOn model" "${model_source}" \
	"${repo_root}/cmd/seek/rerank_assets/model_int8.onnx.zst"
package_asset "tracked LateOn tokenizer" "${tokenizer_source}" \
	"${repo_root}/cmd/seek/rerank_assets/tokenizer.json.zst"
package_asset "tracked ONNX Runtime darwin-amd64" "${ort_darwin_amd64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_amd64/libonnxruntime.dylib.zst"
package_asset "tracked ONNX Runtime darwin-arm64" "${ort_darwin_arm64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_arm64/libonnxruntime.dylib.zst"
package_asset "tracked ONNX Runtime linux-amd64" "${ort_linux_amd64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_linux_amd64/libonnxruntime.so.zst"
package_asset "tracked ONNX Runtime linux-arm64" "${ort_linux_arm64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_linux_arm64/libonnxruntime.so.zst"

write_runtime_manifest "darwin-amd64" "libonnxruntime.dylib" \
	"${ort_darwin_amd64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_amd64/runtime.json"
write_runtime_manifest "darwin-arm64" "libonnxruntime.dylib" \
	"${ort_darwin_arm64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_arm64/runtime.json"
write_runtime_manifest "linux-amd64" "libonnxruntime.so" \
	"${ort_linux_amd64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_linux_amd64/runtime.json"
write_runtime_manifest "linux-arm64" "libonnxruntime.so" \
	"${ort_linux_arm64_source}" \
	"${repo_root}/cmd/seek/rerank_assets_linux_arm64/runtime.json"

echo "Review and commit the tracked resources. Build and release targets do not run this command."
