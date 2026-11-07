package preprocess

import (
	"image"
	"math"
	"sort"
)

// Variant is one OCR-ready rendering of the aligned component crop. All
// variants are dark text on a light background, which is what Tesseract
// expects.
type Variant struct {
	Name  string
	Image *image.Gray
}

// Polarity describes the marking contrast relative to the package body.
type Polarity int

const (
	// PolarityAuto picks the polarity from the body brightness histogram.
	PolarityAuto Polarity = iota
	// LightOnDark is the common case: laser-etched / white print on black
	// epoxy.
	LightOnDark
	// DarkOnLight is e.g. black print on a tan or white package.
	DarkOnLight
)

func (p Polarity) String() string {
	switch p {
	case LightOnDark:
		return "light-on-dark"
	case DarkOnLight:
		return "dark-on-light"
	}
	return "auto"
}

// EnhanceOptions controls how OCR variants are produced from a crop.
type EnhanceOptions struct {
	// TopHatRadius is the radius of the top-hat structuring element in
	// pixels. It must exceed half the stroke width of the marking. Zero
	// selects 1/18 of the crop's short side.
	TopHatRadius int
	// Polarity of the marking. Zero means auto-detect.
	Polarity Polarity
	// CharHeight is the character size, in pixels, that the text variants
	// are rescaled to. Measured on real photos, 40-56 px works best with
	// the stock Tesseract model. Default 44.
	CharHeight int
}

func (o EnhanceOptions) charHeight() float64 {
	if o.CharHeight <= 0 {
		return 44
	}
	return float64(o.CharHeight)
}

// Enhanced is the output of Enhance.
type Enhanced struct {
	Variants []Variant
	// BodyMask marks the package body inside the crop.
	BodyMask *Mask
	// Polarity actually used.
	Polarity Polarity
	// Text is the detected marking layout, nil if none was found.
	Text *TextLayout
}

// Enhance builds OCR variants from an aligned colour crop of the component.
//
// Steps:
//   - locate the package body inside the crop (dark or coloured pixels as
//     opposed to white paper and shiny leads) and shrink it slightly so the
//     package outline is excluded;
//   - decide whether the marking is lighter or darker than the body;
//   - apply a top-hat transform (image minus its morphological opening, on
//     data where the marking is bright). It keeps thin structures such as
//     character strokes and removes everything wider than the structuring
//     element: glare gradients, the body itself, the white paper;
//   - threshold it and keep only character-like connected components;
//   - crop to the text and rescale so characters are CharHeight tall.
//
// Variants, best first:
//
//	"clean"  - the kept character components, black on white;
//	"soft"   - the top-hat image around the kept components (keeps
//	           anti-aliasing and faint strokes the threshold lost);
//	"tophat" - the whole top-hat image of the body (fallback when the
//	           component filter removed real text).
func Enhance(crop image.Image, opt EnhanceOptions) *Enhanced {
	c := toRGBA(crop)
	lum := luminance(c)
	short := min(lum.W, lum.H)
	r := opt.TopHatRadius
	if r <= 0 {
		r = max(2, short/18)
	}

	body := bodyMask(c, short)

	// Approximate Gaussian smoothing against JPEG noise and epoxy grain.
	smooth := boxBlur(boxBlur(lum, 1), 1)

	pol := opt.Polarity
	out := &Enhanced{BodyMask: body}
	if pol != PolarityAuto {
		_, disp, text := analyzePolarity(smooth, body, r, pol)
		out.Polarity, out.Text = pol, text
		out.Variants = buildVariants(disp, text, opt.charHeight(), "")
		return out
	}

	// Glare can cover half the body and fool any histogram test, so try
	// both polarities and rank them by how consistent the character-like
	// components found in each top-hat are; the histogram skew breaks
	// ties. When the call is not clear-cut, variants for the other
	// polarity are appended too (suffix "-inv") and OCR confidence decides.
	_, d1, x1 := analyzePolarity(smooth, body, r, LightOnDark)
	_, d2, x2 := analyzePolarity(smooth, body, r, DarkOnLight)
	q1, q2 := x1.quality(), x2.quality()
	pol = detectPolarity(lum, body)
	switch {
	case q1 > 1.2*q2:
		pol = LightOnDark
	case q2 > 1.2*q1:
		pol = DarkOnLight
	}
	disp, text, odisp, otext := d1, x1, d2, x2
	if pol == DarkOnLight {
		disp, text, odisp, otext = d2, x2, d1, x1
	}
	out.Polarity, out.Text = pol, text
	out.Variants = buildVariants(disp, text, opt.charHeight(), "")
	if math.Max(q1, q2) < 3*math.Min(q1, q2) || (x1 == nil) != (x2 == nil) && text == nil {
		out.Variants = append(out.Variants, buildVariants(odisp, otext, opt.charHeight(), "-inv")...)
	}
	return out
}

// buildVariants renders the OCR variants from a display top-hat (text dark)
// and its text layout. See Enhance for the meaning of each variant.
func buildVariants(disp *Plane, text *TextLayout, target float64, suffix string) []Variant {
	var vs []Variant
	k := 1.0
	if text != nil {
		k = target / text.CharSize
		margin := int(math.Ceil(text.CharSize * 0.6))
		roi := text.Bounds.Inset(-margin)
		pad := int(target)

		clean := NewPlane(text.Mask.W, text.Mask.H)
		for i, v := range text.Mask.Pix {
			if !v {
				clean.Pix[i] = 255
			}
		}
		// "soft": top-hat restricted to the neighbourhood of the kept
		// components.
		near := dilate(text.Mask, max(1, int(text.CharSize/6)))
		soft := disp.Clone()
		paintOutside(soft, near, 255)

		scaled := func(p *Plane) *image.Gray {
			s := subPlane(p, roi)
			s = resample(s, int(math.Round(float64(s.W)*k)), int(math.Round(float64(s.H)*k)))
			return padGray(s.ToGray(), pad)
		}
		vs = append(vs,
			Variant{Name: "clean" + suffix, Image: scaled(clean)},
			Variant{Name: "soft" + suffix, Image: scaled(soft)},
		)
	}
	// Whole-body top-hat, scaled with the same factor when known.
	full := disp
	if text != nil {
		full = resample(disp, int(math.Round(float64(disp.W)*k)), int(math.Round(float64(disp.H)*k)))
	}
	return append(vs, Variant{Name: "tophat" + suffix, Image: padGray(full.ToGray(), max(8, min(full.W, full.H)/10))})
}

// analyzePolarity computes the top-hat for the given marking polarity, a
// display version of it (text dark, stretched from body statistics) and the
// text layout found in it.
func analyzePolarity(smooth *Plane, body *Mask, r int, pol Polarity) (tophat, disp *Plane, text *TextLayout) {
	work := smooth
	if pol == DarkOnLight {
		work = smooth.Clone()
		invert(work)
	}
	opened := maxFilter(minFilter(work, r), r)
	tophat = NewPlane(work.W, work.H)
	for i, v := range work.Pix {
		d := v - opened.Pix[i]
		if d < 0 || !body.Pix[i] {
			d = 0
		}
		tophat.Pix[i] = d
	}
	disp = tophat.Clone()
	stretch(disp, percentileMasked(tophat, body, 0.50), percentileMasked(tophat, body, 0.995))
	invert(disp)
	paintOutside(disp, body, 255)
	return tophat, disp, extractText(tophat, body)
}

// detectPolarity decides whether the marking is lighter or darker than the
// body. Marking pixels are a small minority of the body, so they form the
// longer tail of the body's brightness histogram. Absolute brightness is
// unreliable because glare can make black epoxy look mid-gray.
func detectPolarity(lum *Plane, body *Mask) Polarity {
	q05 := percentileMasked(lum, body, 0.05)
	q50 := percentileMasked(lum, body, 0.50)
	q95 := percentileMasked(lum, body, 0.95)
	if q50-q05 > 1.25*(q95-q50) {
		return DarkOnLight
	}
	return LightOnDark
}

// bodyMask finds the package body inside an aligned crop. min(R,G,B) is low
// for black and for coloured packages and high for white paper and metal.
func bodyMask(c *image.RGBA, short int) *Mask {
	w, h := c.Rect.Dx(), c.Rect.Dy()
	mn := NewPlane(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := c.PixOffset(x, y)
			v := min(c.Pix[o], c.Pix[o+1], c.Pix[o+2])
			mn.Pix[y*w+x] = 255 - float32(v)
		}
	}
	mn = boxBlur(mn, 1)
	m := threshold(mn, otsu(mn))
	m = closeM(m, max(1, short/40))
	labels, comps := components(m)
	best := largest(comps, 0)
	if best == nil || best.Area < w*h/20 {
		// Segmentation failed; fall back to the whole crop.
		all := NewMask(w, h)
		for i := range all.Pix {
			all.Pix[i] = true
		}
		return all
	}
	m = fillHoles(keepLabel(labels, w, h, best.Label))
	// Strip thin protrusions (leads, tab edges) with an opening, then drop
	// the outer rim: package edges and chamfers produce strong gradients
	// that would otherwise survive the top-hat.
	if o := open(m, max(1, short/14)); o.Count() > m.Count()/2 {
		m = o
	}
	return erode(m, max(1, short/50))
}

// padGray surrounds g with a white border; Tesseract segments text touching
// the image edge poorly.
func padGray(g *image.Gray, pad int) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	out := image.NewGray(image.Rect(0, 0, w+2*pad, h+2*pad))
	for i := range out.Pix {
		out.Pix[i] = 255
	}
	for y := 0; y < h; y++ {
		copy(out.Pix[(y+pad)*out.Stride+pad:], g.Pix[g.PixOffset(g.Rect.Min.X, g.Rect.Min.Y+y):][:w])
	}
	return out
}

func paintOutside(p *Plane, m *Mask, v float32) {
	for i, in := range m.Pix {
		if !in {
			p.Pix[i] = v
		}
	}
}

func maskedValues(p *Plane, m *Mask) []float32 {
	var s []float32
	for i, in := range m.Pix {
		if in {
			s = append(s, p.Pix[i])
		}
	}
	return s
}

func percentileMasked(p *Plane, m *Mask, q float64) float32 {
	s := maskedValues(p, m)
	if len(s) == 0 {
		return percentile(p, q)
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(q*float64(len(s)-1))]
}

func otsuMasked(p *Plane, m *Mask) float32 {
	s := maskedValues(p, m)
	if len(s) == 0 {
		return otsu(p)
	}
	return otsu(&Plane{W: len(s), H: 1, Pix: s})
}
