// Package preprocess prepares photos of electronic components for OCR.
//
// The expected input is a photo of a single marked component (IC, MOSFET,
// BJT, regulator, ...) on a white background. Processing steps:
//
//  1. Segment: find the dark component body against the (possibly unevenly
//     lit) white background, strip thin protrusions such as leads and fit a
//     minimum-area rotated rectangle around it.
//  2. Align and crop: resample that rectangle into an axis-aligned image
//     with the long side horizontal, at a normalised resolution.
//  3. Enhance: produce several high-contrast, dark-on-light renderings of
//     the crop suitable for an OCR engine.
//
// The remaining ambiguity (text may be upside down or run along the short
// side) is resolved by the OCR stage, which tries all four orientations.
//
// The package is pure Go and has no dependencies outside the standard
// library.
package preprocess

import (
	"image"
	"math"
)

// Options configures Process.
type Options struct {
	// WorkSize is the longest side, in pixels, of the downscaled image used
	// for segmentation. Default 640.
	WorkSize int
	// MinArea is the smallest body area, as a fraction of the image, that
	// counts as a component. Default 0.004.
	MinArea float64
	// Margin enlarges the detected rectangle on every side by this fraction
	// of its short side. Default 0.04.
	Margin float64
	// TargetHeight is the height (short side) of the aligned crop in pixels.
	// Default 360.
	TargetHeight int
	// Enhance controls variant generation.
	Enhance EnhanceOptions
}

func (o Options) withDefaults() Options {
	if o.WorkSize <= 0 {
		o.WorkSize = 640
	}
	if o.MinArea == 0 {
		o.MinArea = 0.004
	}
	if o.Margin == 0 {
		o.Margin = 0.04
	}
	if o.TargetHeight <= 0 {
		o.TargetHeight = 360
	}
	return o
}

// Result holds every intermediate product of Process.
type Result struct {
	Segmentation *Segmentation
	// Rect is the region that was cropped (segmentation rect plus margin),
	// in source coordinates.
	Rect RotatedRect
	// Crop is the aligned colour crop, long side horizontal.
	Crop *image.RGBA
	// Variants are OCR-ready grayscale renderings of Crop.
	Variants []Variant
	// BodyMask marks the package body inside Crop.
	BodyMask *Mask
	// Polarity is the marking polarity used for enhancement.
	Polarity Polarity
	// Text is the detected marking layout (nil if none was found).
	Text *TextLayout
}

// Process runs segmentation, alignment, cropping and enhancement.
func Process(img image.Image, opt Options) (*Result, error) {
	opt = opt.withDefaults()
	src := toRGBA(img)
	seg, err := Segment(src, opt)
	if err != nil {
		return nil, err
	}
	rect := seg.Rect.Grow(opt.Margin)
	scale := float64(opt.TargetHeight) / math.Max(rect.H, 1)
	crop := ExtractRect(src, rect, scale)
	enh := Enhance(crop, opt.Enhance)
	return &Result{
		Segmentation: seg,
		Rect:         rect,
		Crop:         crop,
		Variants:     enh.Variants,
		BodyMask:     enh.BodyMask,
		Polarity:     enh.Polarity,
		Text:         enh.Text,
	}, nil
}
