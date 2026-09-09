//go:build cgo && darwin && arm64

package main

import _ "embed"

//go:embed rerank_assets_darwin_arm64/libonnxruntime.dylib.zst
var lateOnCompressedRuntime []byte

//go:embed rerank_assets_darwin_arm64/runtime.json
var lateOnRuntimeManifestJSON []byte

func lateOnRuntimeForPlatform() (lateOnRuntimeBundle, error) {
	return lateOnRuntimeBundleFromManifest(
		lateOnRuntimeManifestJSON,
		lateOnCompressedRuntime,
	)
}
