//go:build cgo && linux && amd64

package main

import _ "embed"

//go:embed rerank_assets_linux_amd64/libonnxruntime.so.zst
var lateOnCompressedRuntime []byte

//go:embed rerank_assets_linux_amd64/runtime.json
var lateOnRuntimeManifestJSON []byte

func lateOnRuntimeForPlatform() (lateOnRuntimeBundle, error) {
	return lateOnRuntimeBundleFromManifest(
		lateOnRuntimeManifestJSON,
		lateOnCompressedRuntime,
	)
}
