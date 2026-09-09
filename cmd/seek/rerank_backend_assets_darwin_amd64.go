//go:build cgo && darwin && amd64

package main

import _ "embed"

//go:embed rerank_assets_darwin_amd64/libonnxruntime.dylib.zst
var lateOnCompressedRuntime []byte

//go:embed rerank_assets_darwin_amd64/runtime.json
var lateOnRuntimeManifestJSON []byte

func lateOnRuntimeForPlatform() (lateOnRuntimeBundle, error) {
	return lateOnRuntimeBundleFromManifest(
		lateOnRuntimeManifestJSON,
		lateOnCompressedRuntime,
	)
}
