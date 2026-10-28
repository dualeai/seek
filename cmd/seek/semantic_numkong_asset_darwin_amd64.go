//go:build cgo && darwin && amd64

package main

import _ "embed"

const semanticNumKongRuntimeFileName = "libnumkong.dylib"

//go:embed semantic_assets_darwin_amd64/libnumkong.dylib.zst
var semanticNumKongCompressedRuntime []byte
