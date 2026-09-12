//go:build cgo && linux && amd64

package main

/*
#cgo LDFLAGS: -L${SRCDIR}/rerank_tokenizer_native_linux_amd64
*/
import "C"
