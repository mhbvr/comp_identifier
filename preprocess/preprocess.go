// Package preprocess finds an electronic component in a photo and cuts it
// out, aligned.
//
// The expected input is a photo of a single component (IC, MOSFET, BJT,
// regulator, ...) on a white background. Process:
//
//  1. Segment: find the dark component body against the (possibly unevenly
//     lit) white background, drop shadows and thin protrusions such as
//     leads, and fit a minimum-area rotated rectangle around it.
//  2. Align and crop: resample that rectangle into an axis-aligned image
//     with the long side horizontal, at a normalised height.
//
// Text may still be upside down or run along the short side; the marking
// package resolves that by reading all four orientations.
//
// The package is pure Go and has no dependencies outside the standard
// library.
package preprocess

import (
	"image"
	"math"
)

// Options configures Segment and Process.
type Options struct {
	// WorkSize is the longest side, in pixels, of the downscaled image used
	// for segmentation. Default 640.
	WorkSize int
	// MinArea is the smallest body area, as a fraction of the image, that
	// counts as a component. Default 0.004.
	MinArea float64
	// Margin enlarges the detected rectangle on every side by this fraction
	// of its short side. Default 0.10.
	Margin float64
	// TargetHeight is the height (short side) of the aligned crop in pixels.
	// Default 360.
	TargetHeight int
}

func (o Options) withDefaults() Options {
	if o.WorkSize <= 0 {
		o.WorkSize = 640
	}
	if o.MinArea == 0 {
		o.MinArea = 0.004
	}
	if o.Margin == 0 {
		o.Margin = 0.10
	}
	if o.TargetHeight <= 0 {
		o.TargetHeight = 360
	}
	return o
}

// Result is the output of Process.
type Result struct {
	Segmentation *Segmentation
	// Rect is the region that was cropped (segmentation rect plus margin),
	// in source coordinates.
	Rect RotatedRect
	// Crop is the aligned colour crop, long side horizontal.
	Crop *image.RGBA
}

// Process finds the component in img and returns its aligned crop. It
// returns ErrNoComponent if there is none.
func Process(img image.Image, opt Options) (*Result, error) {
	opt = opt.withDefaults()
	src := toRGBA(img)
	seg, err := Segment(src, opt)
	if err != nil {
		return nil, err
	}
	rect := seg.Rect.Grow(opt.Margin)
	scale := float64(opt.TargetHeight) / math.Max(rect.H, 1)
	return &Result{Segmentation: seg, Rect: rect, Crop: ExtractRect(src, rect, scale)}, nil
}
