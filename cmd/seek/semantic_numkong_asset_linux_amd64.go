//go:build cgo && linux && amd64

package main

import _ "embed"

const semanticNumKongRuntimeFileName = "libnumkong.so"

//go:embed semantic_assets_linux_amd64/libnumkong.so.zst
var semanticNumKongCompressedRuntime []byte
