#!/usr/bin/env bash

# Build the macOS amd64 ONNX Runtime resource that Microsoft does not publish.
# This is a maintainer command. Product builds only use the committed output.
set -euo pipefail

repo_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=cicd/search-assets-common.sh
source "${repo_root}/cicd/search-assets-common.sh"
ort_version=$(search_assets_ort_version)
cache_root=${SEARCH_ASSET_CACHE:-${XDG_CACHE_HOME:-${HOME}/.cache}/seek/search-assets-upgrade}
source_root="${cache_root}/sources"
build_root="${cache_root}/build"
internal_root="${cache_root}/internal"

# This commit must be the release commit for ort_version. Update both values
# together so the locally built resource matches the other platform resources.
ort_commit=f2c39fe2f838cf35ce7da92824f5a5e3ee6e88a7
ort_source_url="https://github.com/microsoft/onnxruntime/archive/${ort_commit}.tar.gz"
ort_source_archive="${source_root}/onnxruntime-${ort_commit}.tar.gz"
ort_source_dir="${source_root}/onnxruntime-${ort_commit}"
ort_build_dir="${build_root}/onnxruntime-${ort_commit}-darwin-amd64"
ort_output_dir="${internal_root}/onnxruntime-osx-x64-${ort_version}"
ort_output="${ort_output_dir}/libonnxruntime.${ort_version}.dylib"

if [[ $(uname -s) != Darwin ]]; then
	fail "this resource build requires macOS"
fi
for command_name in awk file lipo nm wc; do
	command -v "${command_name}" >/dev/null 2>&1 || fail "missing command: ${command_name}"
done
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

download "ONNX Runtime source archive" "${ort_source_url}" "${ort_source_archive}"
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
