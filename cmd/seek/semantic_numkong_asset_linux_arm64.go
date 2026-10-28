//go:build cgo && linux && arm64

package main

import _ "embed"

const semanticNumKongRuntimeFileName = "libnumkong.so"

//go:embed semantic_assets_linux_arm64/libnumkong.so.zst
var semanticNumKongCompressedRuntime []byte
