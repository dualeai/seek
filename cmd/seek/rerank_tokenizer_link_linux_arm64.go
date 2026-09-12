//go:build cgo && linux && arm64

package main

/*
#cgo LDFLAGS: -L${SRCDIR}/rerank_tokenizer_native_linux_arm64
*/
import "C"
