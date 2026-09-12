//go:build cgo && (darwin || linux) && (amd64 || arm64)

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// lateOnStaticRowEncoder owns one ONNX session shared by concurrent Run calls.
// Each call borrows a separate output tensor. outputMu protects only that tensor
// pool; it does not serialize inference. Close requires all Run calls to finish.
type lateOnStaticRowEncoder struct {
	session    *ort.DynamicAdvancedSession
	callCPUs   int
	outputMu   sync.Mutex
	outputFree []*ort.Tensor[float32]
	outputAll  []*ort.Tensor[float32]
}

func newLateOnRowEncoder(ctx context.Context) (*lateOnStaticRowEncoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model, err := decodeLateOnAsset(lateOnCompressedModel)
	if err != nil {
		return nil, fmt.Errorf("load LateOn model: %w", err)
	}
	providerCallCPUs := 1
	if !lateOnProviderHasAccelerator {
		providerCallCPUs = lateOnCPUIntraOpThreads(runtime.GOMAXPROCS(0))
	}
	session, providerErr := newLateOnStaticSession(model, func(options *ort.SessionOptions) error {
		return configureLateOnProvider(options, providerCallCPUs, model)
	})
	if providerErr != nil && lateOnProviderHasAccelerator {
		providerCallCPUs = lateOnCPUIntraOpThreads(runtime.GOMAXPROCS(0))
		var cpuErr error
		session, cpuErr = newLateOnStaticSession(model, func(options *ort.SessionOptions) error {
			return configureLateOnCPUProvider(options, providerCallCPUs)
		})
		if cpuErr != nil {
			return nil, fmt.Errorf(
				"create LateOn session: %w",
				errors.Join(
					fmt.Errorf("accelerated provider: %w", providerErr),
					fmt.Errorf("CPU provider: %w", cpuErr),
				),
			)
		}
		slog.Debug("Core ML session failed; using the CPU provider", "error", providerErr)
	}
	if providerErr != nil && session == nil {
		return nil, providerErr
	}
	accountedCallCPUs := providerCallCPUs
	if lateOnProviderHasAccelerator && providerErr == nil {
		// Treat a Core ML call as accelerator work for shared-gate accounting. Core
		// ML can still use the CPU, GPU, or Neural Engine. The adaptive call controller
		// limits measured throughput and memory use. A zero reservation prevents
		// provider waits from blocking ready tokenizer, ctags, or USearch CPU work.
		accountedCallCPUs = 0
	}
	return &lateOnStaticRowEncoder{session: session, callCPUs: accountedCallCPUs}, nil
}

func newLateOnStaticSession(
	model []byte,
	configure func(*ort.SessionOptions) error,
) (*ort.DynamicAdvancedSession, error) {
	options, err := newLateOnSessionOptions()
	if err != nil {
		return nil, err
	}
	defer func() { _ = options.Destroy() }()
	if err := configure(options); err != nil {
		return nil, err
	}
	session, err := ort.NewDynamicAdvancedSessionWithONNXData(
		model,
		[]string{"input_ids", "attention_mask"},
		[]string{"output"},
		options,
	)
	if err != nil {
		return nil, fmt.Errorf("create LateOn session: %w", err)
	}
	return session, nil
}

func configureLateOnCPUProvider(options *ort.SessionOptions, intraOpThreads int) error {
	intraOpThreads = max(1, intraOpThreads)
	if err := options.SetIntraOpNumThreads(intraOpThreads); err != nil {
		return fmt.Errorf("set LateOn intra-op threads: %w", err)
	}
	if err := options.SetInterOpNumThreads(1); err != nil {
		return fmt.Errorf("set LateOn inter-op threads: %w", err)
	}
	return nil
}

// lateOnCPUIntraOpThreads keeps both ONNX kernel work and independent model
// calls available. Both dimensions grow with the effective CPU count. The
// outer controller then reduces the call count when live memory or measured
// throughput requires it.
func lateOnCPUIntraOpThreads(cpuLimit int) int {
	cpuLimit = max(1, cpuLimit)
	return max(1, int(math.Round(math.Sqrt(float64(cpuLimit)/2))))
}

func (encoder *lateOnStaticRowEncoder) CallCPUs() int {
	if encoder == nil {
		return 1
	}
	return max(0, encoder.callCPUs)
}

func (encoder *lateOnStaticRowEncoder) Run(
	ctx context.Context,
	inputIDs []int64,
	attention []int64,
	rows int,
	consume func([]float32) error,
) error {
	if encoder == nil || encoder.session == nil || rows < 1 || rows > semanticModelBatchRows ||
		len(inputIDs) != rows*lateOnSequenceLength || len(attention) != len(inputIDs) || consume == nil {
		return fmt.Errorf("LateOn batch has an invalid shape")
	}
	paddedIDs := inputIDs
	paddedAttention := attention
	if rows < semanticModelBatchRows {
		paddedIDs = make([]int64, semanticModelBatchRows*lateOnSequenceLength)
		paddedAttention = make([]int64, len(paddedIDs))
		for index := range paddedIDs {
			paddedIDs[index] = lateOnPadTokenID
		}
		copy(paddedIDs, inputIDs)
		copy(paddedAttention, attention)
	}
	output, err := encoder.takeOutput()
	if err != nil {
		return err
	}
	defer encoder.releaseOutput(output)
	if err := runLateOnSessionRows(
		ctx,
		encoder.session,
		paddedIDs,
		paddedAttention,
		semanticModelBatchRows,
		output,
	); err != nil {
		return err
	}
	return consume(output.GetData()[:rows*lateOnSequenceLength*semanticEmbeddingDimensions])
}

func (encoder *lateOnStaticRowEncoder) takeOutput() (*ort.Tensor[float32], error) {
	encoder.outputMu.Lock()
	defer encoder.outputMu.Unlock()
	last := len(encoder.outputFree) - 1
	if last >= 0 {
		output := encoder.outputFree[last]
		encoder.outputFree = encoder.outputFree[:last]
		return output, nil
	}
	shape := ort.NewShape(
		semanticModelBatchRows,
		lateOnSequenceLength,
		semanticEmbeddingDimensions,
	)
	output, err := ort.NewEmptyTensor[float32](shape)
	if err != nil {
		return nil, fmt.Errorf("create LateOn output: %w", err)
	}
	encoder.outputAll = append(encoder.outputAll, output)
	return output, nil
}

func (encoder *lateOnStaticRowEncoder) releaseOutput(output *ort.Tensor[float32]) {
	encoder.outputMu.Lock()
	encoder.outputFree = append(encoder.outputFree, output)
	encoder.outputMu.Unlock()
}

func (encoder *lateOnStaticRowEncoder) Close() error {
	if encoder == nil {
		return nil
	}
	encoder.outputMu.Lock()
	outputs := encoder.outputAll
	encoder.outputAll = nil
	encoder.outputFree = nil
	encoder.outputMu.Unlock()
	var closeErr error
	for _, output := range outputs {
		closeErr = errors.Join(closeErr, output.Destroy())
	}
	if encoder.session != nil {
		closeErr = errors.Join(closeErr, encoder.session.Destroy())
		encoder.session = nil
	}
	return closeErr
}
