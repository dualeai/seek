//go:build cgo && darwin && amd64

package main

/*
#cgo LDFLAGS: -L${SRCDIR}/rerank_tokenizer_native_darwin_amd64
*/
import "C"
