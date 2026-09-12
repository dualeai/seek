#!/usr/bin/env bash

# This manual maintainer command updates tracked search resources. Product
# builds consume those resources and never invoke this script or fetch them.
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=cicd/search-assets-common.sh
source "${repo_root}/cicd/search-assets-common.sh"
ort_version=$(search_assets_ort_version)
cache_root=${SEARCH_ASSET_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/seek/search-assets-upgrade}
source_dir="${cache_root}/sources"
work_dir="${cache_root}/work"

revision=4bcdf5ed93f791259eb130b577a240f753d68dd8
model_url="https://huggingface.co/lightonai/LateOn-Code-edge/resolve/${revision}/model.onnx"
tokenizer_url="https://huggingface.co/lightonai/LateOn-Code-edge/resolve/${revision}/tokenizer.json"

tokenizers_version=1.27.0
tokenizers_release="https://github.com/daulet/tokenizers/releases/download/v${tokenizers_version}"
tokenizers_darwin_amd64_url="${tokenizers_release}/libtokenizers.darwin-x86_64.tar.gz"
tokenizers_darwin_arm64_url="${tokenizers_release}/libtokenizers.darwin-arm64.tar.gz"
tokenizers_linux_amd64_url="${tokenizers_release}/libtokenizers.linux-amd64.tar.gz"
tokenizers_linux_arm64_url="${tokenizers_release}/libtokenizers.linux-arm64.tar.gz"

usearch_version=2.26.2
usearch_release="https://github.com/unum-cloud/USearch/releases/download/v${usearch_version}"
usearch_darwin_amd64_url="${usearch_release}/usearch_macos_x86_64_${usearch_version}.zip"
usearch_darwin_arm64_url="${usearch_release}/usearch_macos_arm64_${usearch_version}.zip"
usearch_linux_amd64_url="${usearch_release}/usearch_linux_amd64_${usearch_version}.so"
usearch_linux_arm64_url="${usearch_release}/usearch_linux_arm64_${usearch_version}.so"

ort_darwin_arm64_url="https://github.com/microsoft/onnxruntime/releases/download/v${ort_version}/onnxruntime-osx-arm64-${ort_version}.tgz"
ort_linux_amd64_url="https://github.com/microsoft/onnxruntime/releases/download/v${ort_version}/onnxruntime-linux-x64-${ort_version}.tgz"
ort_linux_arm64_url="https://github.com/microsoft/onnxruntime/releases/download/v${ort_version}/onnxruntime-linux-aarch64-${ort_version}.tgz"

for command_name in awk cmp curl tar unzip uv wc zstd; do
	command -v "${command_name}" >/dev/null 2>&1 || fail "missing command: ${command_name}"
done
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

track_release_archive() {
	local label=$1
	local source=$2
	local destination=$3
	local temporary="${destination}.tmp.$$"

	cp "${source}" "${temporary}"
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

extract_zip_member() {
	local label=$1
	local archive=$2
	local member=$3
	local destination=$4
	local temporary="${destination}.tmp.$$"

	unzip -p "${archive}" "${member}" >"${temporary}"
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

model_source="${source_dir}/LateOn-Code-edge-${revision}-model.onnx"
model_fp16_source="${work_dir}/LateOn-Code-edge-${revision}-static-fp16-b128.onnx"
tokenizer_source="${source_dir}/LateOn-Code-edge-${revision}-tokenizer.json"
ort_darwin_amd64_source="${cache_root}/internal/onnxruntime-osx-x64-${ort_version}/libonnxruntime.${ort_version}.dylib"
ort_darwin_arm64_archive="${source_dir}/onnxruntime-osx-arm64-${ort_version}.tgz"
ort_linux_amd64_archive="${source_dir}/onnxruntime-linux-x64-${ort_version}.tgz"
ort_linux_arm64_archive="${source_dir}/onnxruntime-linux-aarch64-${ort_version}.tgz"
ort_darwin_arm64_source="${work_dir}/libonnxruntime-darwin-arm64-${ort_version}.dylib"
ort_linux_amd64_source="${work_dir}/libonnxruntime-linux-amd64-${ort_version}.so"
ort_linux_arm64_source="${work_dir}/libonnxruntime-linux-arm64-${ort_version}.so"
tokenizers_darwin_amd64_archive="${source_dir}/libtokenizers-${tokenizers_version}-darwin-amd64.tar.gz"
tokenizers_darwin_arm64_archive="${source_dir}/libtokenizers-${tokenizers_version}-darwin-arm64.tar.gz"
tokenizers_linux_amd64_archive="${source_dir}/libtokenizers-${tokenizers_version}-linux-amd64.tar.gz"
tokenizers_linux_arm64_archive="${source_dir}/libtokenizers-${tokenizers_version}-linux-arm64.tar.gz"
usearch_darwin_amd64_archive="${source_dir}/usearch-macos-x86_64-${usearch_version}.zip"
usearch_darwin_arm64_archive="${source_dir}/usearch-macos-arm64-${usearch_version}.zip"
usearch_darwin_amd64_source="${work_dir}/libusearch-darwin-amd64-${usearch_version}.dylib"
usearch_darwin_arm64_source="${work_dir}/libusearch-darwin-arm64-${usearch_version}.dylib"
usearch_linux_amd64_source="${source_dir}/libusearch-linux-amd64-${usearch_version}.so"
usearch_linux_arm64_source="${source_dir}/libusearch-linux-arm64-${usearch_version}.so"

download "LateOn model" "${model_url}" "${model_source}"
download "LateOn tokenizer" "${tokenizer_url}" "${tokenizer_source}"
download "ONNX Runtime darwin-arm64 archive" "${ort_darwin_arm64_url}" \
	"${ort_darwin_arm64_archive}"
download "ONNX Runtime linux-amd64 archive" "${ort_linux_amd64_url}" \
	"${ort_linux_amd64_archive}"
download "ONNX Runtime linux-arm64 archive" "${ort_linux_arm64_url}" \
	"${ort_linux_arm64_archive}"
download "tokenizers darwin-amd64 archive" "${tokenizers_darwin_amd64_url}" \
	"${tokenizers_darwin_amd64_archive}"
download "tokenizers darwin-arm64 archive" "${tokenizers_darwin_arm64_url}" \
	"${tokenizers_darwin_arm64_archive}"
download "tokenizers linux-amd64 archive" "${tokenizers_linux_amd64_url}" \
	"${tokenizers_linux_amd64_archive}"
download "tokenizers linux-arm64 archive" "${tokenizers_linux_arm64_url}" \
	"${tokenizers_linux_arm64_archive}"
download "USearch darwin-amd64 archive" "${usearch_darwin_amd64_url}" \
	"${usearch_darwin_amd64_archive}"
download "USearch darwin-arm64 archive" "${usearch_darwin_arm64_url}" \
	"${usearch_darwin_arm64_archive}"
download "USearch linux-amd64 library" "${usearch_linux_amd64_url}" \
	"${usearch_linux_amd64_source}"
download "USearch linux-arm64 library" "${usearch_linux_arm64_url}" \
	"${usearch_linux_arm64_source}"

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
extract_zip_member "USearch darwin-amd64 library" \
	"${usearch_darwin_amd64_archive}" "libusearch_c.dylib" \
	"${usearch_darwin_amd64_source}"
extract_zip_member "USearch darwin-arm64 library" \
	"${usearch_darwin_arm64_archive}" "libusearch_c.dylib" \
	"${usearch_darwin_arm64_source}"

uv run --script "${repo_root}/cicd/lateon-static-fp16.py" \
	"${model_source}" "${model_fp16_source}"

mkdir -p \
	"${repo_root}/cmd/seek/rerank_assets" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_amd64" \
	"${repo_root}/cmd/seek/rerank_assets_darwin_arm64" \
	"${repo_root}/cmd/seek/rerank_assets_linux_amd64" \
	"${repo_root}/cmd/seek/rerank_assets_linux_arm64" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_darwin_amd64" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_darwin_arm64" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_linux_amd64" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_linux_arm64" \
	"${repo_root}/cmd/seek/semantic_assets_darwin_amd64" \
	"${repo_root}/cmd/seek/semantic_assets_darwin_arm64" \
	"${repo_root}/cmd/seek/semantic_assets_linux_amd64" \
	"${repo_root}/cmd/seek/semantic_assets_linux_arm64"
package_asset "tracked LateOn static FP16 model" "${model_fp16_source}" \
	"${repo_root}/cmd/seek/rerank_assets/model_fp16_static_b128.onnx.zst"
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
package_asset "tracked USearch darwin-amd64" "${usearch_darwin_amd64_source}" \
	"${repo_root}/cmd/seek/semantic_assets_darwin_amd64/libusearch_c.dylib.zst"
package_asset "tracked USearch darwin-arm64" "${usearch_darwin_arm64_source}" \
	"${repo_root}/cmd/seek/semantic_assets_darwin_arm64/libusearch_c.dylib.zst"
package_asset "tracked USearch linux-amd64" "${usearch_linux_amd64_source}" \
	"${repo_root}/cmd/seek/semantic_assets_linux_amd64/libusearch_c.so.zst"
package_asset "tracked USearch linux-arm64" "${usearch_linux_arm64_source}" \
	"${repo_root}/cmd/seek/semantic_assets_linux_arm64/libusearch_c.so.zst"

# Keep the upstream release archives unchanged. Make extracts the current
# target before Go links the native tokenizer into Seek.
track_release_archive "tracked tokenizers darwin-amd64 archive" \
	"${tokenizers_darwin_amd64_archive}" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_darwin_amd64/libtokenizers.tar.gz"
track_release_archive "tracked tokenizers darwin-arm64 archive" \
	"${tokenizers_darwin_arm64_archive}" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_darwin_arm64/libtokenizers.tar.gz"
track_release_archive "tracked tokenizers linux-amd64 archive" \
	"${tokenizers_linux_amd64_archive}" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_linux_amd64/libtokenizers.tar.gz"
track_release_archive "tracked tokenizers linux-arm64 archive" \
	"${tokenizers_linux_arm64_archive}" \
	"${repo_root}/cmd/seek/rerank_tokenizer_assets_linux_arm64/libtokenizers.tar.gz"

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
