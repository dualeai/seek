#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10,<3.13"
# dependencies = [
#   "numpy==2.0.2",
#   "onnx==1.17.0",
#   "onnxconverter-common==1.14.0",
# ]
# ///

"""Create the static FP16 LateOn model used by the production Go runtime.

Run this maintainer tool through uv run --script. The fixed shapes below
form the model file interface. They are not inference concurrency targets and
must match the corresponding constants in cmd/seek.
"""

import argparse
from pathlib import Path

import onnx
from onnx import TensorProto, helper
from onnxconverter_common import float16


# These format values must match semanticModelBatchRows, lateOnSequenceLength,
# and semanticEmbeddingDimensions in the Go runtime.
BATCH_ROWS = 128
SEQUENCE_TOKENS = 128
EMBEDDING_DIMENSIONS = 48
ROTARY_NEGATIVE_ONE = "seek.rotary.negative_one_fp16"


def set_dimension(dimension, value):
    dimension.ClearField("dim_param")
    dimension.dim_value = value


def replace_rotary_negation(model):
    """Keep the pinned LateOn rotary operations in CoreML FP16 partitions.

    The node-name match is specific to the pinned source graph. Review this
    rewrite when the model revision or CoreML provider changes.
    """
    if any(value.name == ROTARY_NEGATIVE_ONE for value in model.graph.initializer):
        raise ValueError(f"initializer already exists: {ROTARY_NEGATIVE_ONE}")
    changed = 0
    for node in model.graph.node:
        if node.op_type != "Neg" or "/attn/Neg" not in node.name:
            continue
        node.op_type = "Mul"
        node.input.append(ROTARY_NEGATIVE_ONE)
        changed += 1
    if changed == 0:
        raise ValueError("model has no rotary attention negation")
    model.graph.initializer.append(
        helper.make_tensor(ROTARY_NEGATIVE_ONE, TensorProto.FLOAT16, [], [-1.0])
    )


def make_model(source):
    model = onnx.load(source)
    for value in model.graph.input:
        dimensions = value.type.tensor_type.shape.dim
        set_dimension(dimensions[0], BATCH_ROWS)
        set_dimension(dimensions[1], SEQUENCE_TOKENS)
    dimensions = model.graph.output[0].type.tensor_type.shape.dim
    set_dimension(dimensions[0], BATCH_ROWS)
    set_dimension(dimensions[1], SEQUENCE_TOKENS)
    set_dimension(dimensions[2], EMBEDDING_DIMENSIONS)
    model = float16.convert_float_to_float16(
        model,
        keep_io_types=True,
        disable_shape_infer=False,
    )
    # Core ML does not retain these small Neg nodes. It returns to the CPU and
    # adds two precision casts for each node. FP16 multiplication is exact for
    # -1 and lets Core ML keep the transformer layers in larger partitions.
    replace_rotary_negation(model)
    onnx.checker.check_model(model)
    return model


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path, help="source LateOn ONNX model")
    parser.add_argument("output", type=Path, help="destination static FP16 ONNX model")
    args = parser.parse_args()
    model = make_model(args.input)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    onnx.save_model(model, args.output)
    onnx.checker.check_model(args.output)


if __name__ == "__main__":
    main()
