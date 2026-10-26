//go:build cgo && ((darwin && amd64) || (linux && (amd64 || arm64)))

package main

import (
	"context"

	ort "github.com/yalue/onnxruntime_go"
)

const lateOnProviderHasAccelerator = false

// lateOnAcceleratedProviderName keeps one name for both platform files. These
// targets have no accelerated provider, so it names the CPU provider. Shared
// code still reads it. lateOnProviderHasAccelerator guards every branch that
// would prefer an accelerator, so two names with one value change no behavior.
const lateOnAcceleratedProviderName = "cpu"

// configureLateOnProvider configures the CPU provider, the only provider of
// these targets. It needs no model bytes.
func configureLateOnProvider(options *ort.SessionOptions, callCPUs int, _ []byte) error {
	return configureLateOnCPUProvider(options, callCPUs)
}

// rejectLateOnAcceleratedProvider has no accelerated provider to reject, and no
// second provider to move to.
func rejectLateOnAcceleratedProvider(_ string) {}

// cachedLateOnProviderVerdict has nothing to report on these targets.
func cachedLateOnProviderVerdict(_ []byte) string { return "" }

// verifyLateOnAcceleratedProvider has nothing to verify on these targets. The
// CPU provider is the only one, so there is no second provider to compare it
// with and nothing to fall back to.
func verifyLateOnAcceleratedProvider(
	_ context.Context,
	_ *lateOnStaticRowEncoder,
	_ []byte,
	_ *ort.DynamicAdvancedSession,
) (bool, string, error) {
	return true, "only provider", nil
}
