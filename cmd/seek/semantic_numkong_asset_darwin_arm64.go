//go:build cgo && darwin && arm64

package main

import _ "embed"

const semanticNumKongRuntimeFileName = "libnumkong.dylib"

//go:embed semantic_assets_darwin_arm64/libnumkong.dylib.zst
var semanticNumKongCompressedRuntime []byte
