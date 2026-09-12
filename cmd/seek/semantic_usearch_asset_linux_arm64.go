//go:build cgo && linux && arm64

package main

import _ "embed"

const semanticUSearchRuntimeFileName = "libusearch_c.so"

//go:embed semantic_assets_linux_arm64/libusearch_c.so.zst
var semanticUSearchCompressedRuntime []byte
