package ocr

import (
	"os"
	"syscall"
)

// LimitOpenMPThreads restarts the current process with OMP_THREAD_LIMIT=1
// unless that variable is already set. Call it first thing in main (or
// TestMain).
//
// Tesseract parallelises with OpenMP, which on images as small as a
// component crop costs far more than it gains: OCR runs about 4-5x slower.
// libgomp reads the limit only when the process starts, hence the re-exec.
// On failure it returns and the process continues unchanged.
func LimitOpenMPThreads() {
	if os.Getenv("OMP_THREAD_LIMIT") != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = syscall.Exec(exe, os.Args, append(os.Environ(), "OMP_THREAD_LIMIT=1"))
}
