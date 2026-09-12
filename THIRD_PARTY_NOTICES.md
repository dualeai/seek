# Third-party notices

Seek includes the resources below in its release executable. The release SBOM
lists the Go modules used to build the executable. The release archive includes
this file and Seek's Apache-2.0 `LICENSE` file.

## LateOn-Code-edge

- Source: <https://huggingface.co/lightonai/LateOn-Code-edge/tree/4bcdf5ed93f791259eb130b577a240f753d68dd8>
- Included resources: model and tokenizer
- License: Apache License 2.0

Seek changes the source ONNX model to fixed batch and sequence shapes and
converts its floating-point weights to FP16. The conversion source is
`cicd/lateon-static-fp16.py`.

## USearch 2.26.2

- Source: <https://github.com/unum-cloud/USearch/tree/f91fe5bc000222aa1af6e91daf78c2bb20b0c90e>
- Included resources: native C libraries for the four release targets
- License: Apache License 2.0

The full Apache-2.0 terms are in Seek's `LICENSE` file.

## ONNX Runtime 1.29.0

- Source: <https://github.com/microsoft/onnxruntime/tree/v1.29.0>
- Included resources: native runtime libraries for the four release targets
- License: MIT
- Included upstream notices: [ONNXRUNTIME_THIRD_PARTY_NOTICES.txt](ONNXRUNTIME_THIRD_PARTY_NOTICES.txt)
- Notice source: <https://github.com/microsoft/onnxruntime/blob/v1.29.0/ThirdPartyNotices.txt>

Copyright (c) Microsoft Corporation

## daulet/tokenizers 1.27.0

- Source: <https://github.com/daulet/tokenizers/tree/v1.27.0>
- Included resources: native static libraries for the four release targets
- License: MIT

Copyright (c) 2023 Daulet Zhanguzin

## MIT license text

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
