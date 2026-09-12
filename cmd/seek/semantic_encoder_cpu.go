//go:build cgo && ((darwin && amd64) || (linux && (amd64 || arm64)))

package main

import ort "github.com/yalue/onnxruntime_go"

const lateOnProviderHasAccelerator = false

func configureLateOnProvider(options *ort.SessionOptions, callCPUs int, _ []byte) error {
	return configureLateOnCPUProvider(options, callCPUs)
}
