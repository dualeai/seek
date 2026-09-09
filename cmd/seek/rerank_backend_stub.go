//go:build !cgo || (!darwin && !linux) || (!amd64 && !arm64)

package main

import "context"

func rerankBackendBundled() bool {
	return false
}

func newLateOnRerankScorer(context.Context) (rerankScorer, error) {
	return nil, errRerankUnavailable
}
