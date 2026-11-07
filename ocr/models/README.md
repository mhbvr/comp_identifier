# OCR models

The files here are embedded into the binary (see `ocr/models.go`).

| file | model | source | SHA-256 |
|---|---|---|---|
| `det_ppocrv3.onnx` | PaddleOCR PP-OCRv3 text detector (DB), "ch" | `ch_PP-OCRv3_det_infer.onnx` from the [RapidOCR](https://github.com/RapidAI/RapidOCR) Python package `rapidocr_onnxruntime` 1.2.3 | `3439588c030faea393a54515f51e983d8e155b19a2e8aba7891934c1cf0de526` |
| `rec_en_ppocrv5.onnx` | PaddleOCR PP-OCRv5 English text recogniser (`en_PP-OCRv5_mobile_rec`) | `languages/english/rec.onnx` from [monkt/paddleocr-onnx](https://huggingface.co/monkt/paddleocr-onnx) | `4e16deb22c4da6468bdca539b2cd3c8687825538b67109177c47d359ab994cd7` |
| `dict_en_ppocrv5.txt` | character dictionary of the recogniser (436 characters) | `languages/english/dict.txt` from the same repository | `e025a66d31f327ba0c232e03f407ae8d105e1e709e7ccb3f408aa778c24e70d6` |

All three are conversions of [PaddleOCR](https://github.com/PaddlePaddle/PaddleOCR) models to ONNX and
are distributed under the Apache License 2.0, like PaddleOCR itself.

Why these two: see [doc/text-recognition.md](../../doc/text-recognition.md).
