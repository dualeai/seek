//go:build cgo && darwin && amd64

package main

import _ "embed"

const semanticUSearchRuntimeFileName = "libusearch_c.dylib"

//go:embed semantic_assets_darwin_amd64/libusearch_c.dylib.zst
var semanticUSearchCompressedRuntime []byte
