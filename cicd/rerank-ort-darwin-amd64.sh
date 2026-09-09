#!/usr/bin/env bash

# Build the macOS amd64 ONNX Runtime resource that Microsoft does not publish.
# This is a maintainer command. Product builds only use the committed output.
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cache_root=${RERANK_ASSET_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/seek/rerank-assets-upgrade}
source_root="${cache_root}/sources"
build_root="${cache_root}/build"
internal_root="${cache_root}/internal"

ort_version=1.29.0
ort_commit=2e2543fbe9fae542f921d47a72d21d5a4ef0b710
ort_source_url="https://github.com/microsoft/onnxruntime/archive/${ort_commit}.tar.gz"
ort_source_archive="${source_root}/onnxruntime-${ort_commit}.tar.gz"
ort_source_dir="${source_root}/onnxruntime-${ort_commit}"
ort_build_dir="${build_root}/onnxruntime-${ort_commit}-darwin-amd64"
ort_output_dir="${internal_root}/onnxruntime-osx-x64-${ort_version}"
ort_output="${ort_output_dir}/libonnxruntime.${ort_version}.dylib"

fail() {
	echo "rerank-ort-darwin-amd64: $*" >&2
	exit 1
}

if [[ $(uname -s) != Darwin ]]; then
	fail "this resource build requires macOS"
fi
for command_name in awk file lipo nm wc; do
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
	local url=$1
	local destination=$2
	local partial="${destination}.part"

	if [[ -f ${destination} ]]; then
		echo "Using cached ONNX Runtime source: ${destination}"
		show_metadata "ONNX Runtime source archive" "${destination}"
		return
	fi
	echo "Downloading ONNX Runtime source: ${url}"
	if ! curl --fail --location --silent --show-error --continue-at - \
		--connect-timeout 30 --retry 5 --retry-all-errors --retry-delay 1 \
		--output "${partial}" "${url}"; then
		fail "download failed; run this command again to resume ${partial}"
	fi
	mv -f "${partial}" "${destination}"
	chmod 0644 "${destination}"
	show_metadata "ONNX Runtime source archive" "${destination}"
}

validate_runtime() {
	[[ $(lipo -archs "$1") == x86_64 ]] || fail "runtime is not macOS amd64: $1"
	file "$1" | awk '/Mach-O 64-bit dynamically linked shared library x86_64/ {found=1} END {exit !found}' ||
		fail "runtime is not an x86_64 dylib: $1"
	nm -gU "$1" | awk '$3 == "_OrtGetApiBase" {found=1} END {exit !found}' ||
		fail "runtime does not export OrtGetApiBase: $1"
}

mkdir -p "${source_root}" "${build_root}" "${internal_root}"
if [[ -f ${ort_output} ]]; then
	validate_runtime "${ort_output}"
	echo "Using cached ONNX Runtime darwin-amd64 build: ${ort_output}"
	show_metadata "ONNX Runtime darwin-amd64 source" "${ort_output}"
	exit 0
fi

for command_name in cmake curl ninja tar; do
	command -v "${command_name}" >/dev/null 2>&1 || fail "missing command: ${command_name}"
done
python_command=${PYTHON:-$(command -v python3 || true)}
[[ -n ${python_command} ]] || fail "missing Python 3.10 or newer"
python_version=$(
	"${python_command}" -c 'import sys; print(sys.version_info.major * 100 + sys.version_info.minor)'
) || fail "cannot read the Python version"
((python_version >= 310)) || fail "Python 3.10 or newer is required"

download "${ort_source_url}" "${ort_source_archive}"
if [[ ! -x ${ort_source_dir}/build.sh ]]; then
	temporary_source=$(mktemp -d "${source_root}/onnxruntime-source.XXXXXX")
	tar -xzf "${ort_source_archive}" -C "${temporary_source}" --strip-components=1
	mv -f "${temporary_source}" "${ort_source_dir}"
fi

"${python_command}" "${ort_source_dir}/tools/ci_build/build.py" \
	--build_dir "${ort_build_dir}" \
	--config Release \
	--update \
	--build \
	--target onnxruntime \
	--skip_tests \
	--skip_submodule_sync \
	--build_shared_lib \
	--parallel \
	--compile_no_warning_as_error \
	--no_telemetry \
	--cmake_generator Ninja \
	--cmake_extra_defines \
		CMAKE_OSX_ARCHITECTURES=x86_64 \
		CMAKE_OSX_DEPLOYMENT_TARGET=14.0 \
		CMAKE_POLICY_VERSION_MINIMUM=3.5

built_runtime="${ort_build_dir}/Release/libonnxruntime.${ort_version}.dylib"
[[ -f ${built_runtime} ]] || fail "build did not produce ${built_runtime}"
validate_runtime "${built_runtime}"
mkdir -p "${ort_output_dir}"
temporary_output="${ort_output}.tmp.$$"
cp "${built_runtime}" "${temporary_output}"
chmod 0755 "${temporary_output}"
mv -f "${temporary_output}" "${ort_output}"
show_metadata "ONNX Runtime darwin-amd64 source" "${ort_output}"

echo "The committed compressed resource is the pinned internal build output."
echo "Repository: ${repo_root}"
