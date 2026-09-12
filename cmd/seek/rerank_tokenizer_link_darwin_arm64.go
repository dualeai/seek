//go:build cgo && darwin && arm64

package main

/*
#cgo LDFLAGS: -L${SRCDIR}/rerank_tokenizer_native_darwin_arm64
*/
import "C"
