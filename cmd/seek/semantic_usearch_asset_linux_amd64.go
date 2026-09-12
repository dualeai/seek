//go:build cgo && linux && amd64

package main

import _ "embed"

const semanticUSearchRuntimeFileName = "libusearch_c.so"

//go:embed semantic_assets_linux_amd64/libusearch_c.so.zst
var semanticUSearchCompressedRuntime []byte
