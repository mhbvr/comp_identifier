//go:build !cgo

package ocr

// NewEngine returns the preferred engine for this build. Without cgo the
// tesseract command-line tool is used.
func NewEngine(cfg Config) (Engine, error) { return NewCLIEngine(cfg) }
