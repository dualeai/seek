//go:build cgo && darwin && arm64

package main

import _ "embed"

const semanticUSearchRuntimeFileName = "libusearch_c.dylib"

//go:embed semantic_assets_darwin_arm64/libusearch_c.dylib.zst
var semanticUSearchCompressedRuntime []byte
