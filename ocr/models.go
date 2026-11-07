package ocr

import _ "embed"

// The models are embedded so that the binary needs nothing but the ONNX
// Runtime library. Sources and licences: models/README.md.

//go:embed models/det_ppocrv3.onnx
var detModel []byte

//go:embed models/rec_en_ppocrv5.onnx
var recModel []byte

//go:embed models/dict_en_ppocrv5.txt
var dictText string
