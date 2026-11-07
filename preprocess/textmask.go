package preprocess

import (
	"image"
	"math"
	"sort"
)

// TextLayout describes the marking found on the body.
type TextLayout struct {
	// Mask holds the character-like components that were kept, in crop
	// coordinates.
	Mask *Mask
	// Bounds is the bounding box of all kept components.
	Bounds image.Rectangle
	// CharSize is the typical extent of characters across the text line
	// (the cap height), in crop pixels.
	CharSize float64
	// Vertical is true when text lines run along the crop's Y axis, i.e.
	// the text must be rotated by 90 or 270 degrees to be read.
	Vertical bool
	// Count is the number of kept components.
	Count int
	// Consistency is the area-weighted fraction of candidate blobs whose
	// across-line size is within 25% of CharSize: high for real text,
	// low for texture, glare edges and other clutter.
	Consistency float64
}

// quality rates how text-like the layout is (0 for nil).
func (t *TextLayout) quality() float64 {
	if t == nil {
		return 0
	}
	return float64(t.Count) * t.Consistency
}

// extractText finds marking characters in a "marking is bright" top-hat
// image restricted to body.
//
// A two-level threshold (strong = Otsu over body pixels, weak = a fraction
// of it, accepted only next to strong pixels) yields full strokes without
// picking up noise. Connected components are then filtered so that only
// character-like blobs remain: not touching the body outline (lead stubs,
// chamfers), not tiny (dust, texture) and of a size consistent with the
// text height.
//
// Neighbouring characters often touch and merge into word-sized blobs, so
// size is measured across the text line, the one dimension that is the
// same for single characters and merged words. Which image axis that is
// (i.e. whether the text runs horizontally or vertically in the crop) is
// decided by which bounding-box dimension is more consistent over the
// candidate blobs.
func extractText(tophat *Plane, body *Mask) *TextLayout {
	w, h := tophat.W, tophat.H
	short := min(w, h)
	t := textThreshold(tophat, body)
	strong := NewMask(w, h)
	weak := NewMask(w, h)
	for i, v := range tophat.Pix {
		if !body.Pix[i] {
			continue
		}
		strong.Pix[i] = v > t
		weak.Pix[i] = v > 0.6*t
	}
	// Grow strong strokes into adjacent weak pixels only (constrained
	// hysteresis). Unbounded hysteresis would bridge characters to grain
	// noise and to the package edge highlight.
	grow := dilate(strong, max(1, short/150))
	mask := NewMask(w, h)
	for i := range mask.Pix {
		mask.Pix[i] = strong.Pix[i] || (weak.Pix[i] && grow.Pix[i])
	}

	labels, comps := components(mask)
	edge := bodyEdge(body)
	touches := make([]bool, len(comps))
	for i, l := range labels {
		if l != 0 && edge.Pix[i] {
			touches[l-1] = true
		}
	}

	minArea := max(6, short*short/5000)
	type cand struct {
		idx    int
		bw, bh float64
		area   float64
	}
	var cands, plausible []cand
	for i, c := range comps {
		if touches[i] || c.Area < minArea {
			continue
		}
		cd := cand{i, float64(c.MaxX - c.MinX + 1), float64(c.MaxY - c.MinY + 1), float64(c.Area)}
		cands = append(cands, cd)
		if s := math.Max(cd.bw, cd.bh); s >= float64(short)/20 && math.Min(cd.bw, cd.bh) <= float64(short)*0.5 {
			plausible = append(plausible, cd)
		}
	}
	if len(plausible) == 0 {
		return nil
	}

	// Pick the across-line dimension: the one whose values cluster best.
	type dimStat struct{ median, conc float64 }
	stat := func(get func(cand) float64) dimStat {
		vals := make([]float64, len(plausible))
		for i, c := range plausible {
			vals[i] = get(c)
		}
		sort.Float64s(vals)
		m := vals[len(vals)/2]
		var in, tot float64
		for _, c := range plausible {
			v := get(c)
			tot += c.area
			if v >= 0.75*m && v <= 1.33*m {
				in += c.area
			}
		}
		return dimStat{m, in / tot}
	}
	getH := func(c cand) float64 { return c.bh }
	getW := func(c cand) float64 { return c.bw }
	sh, sw := stat(getH), stat(getW)
	vertical := sw.conc > sh.conc
	across, along, charSize := getH, getW, sh.median
	if vertical {
		across, along, charSize = getW, getH, sw.median
	}

	keep := make([]bool, len(comps))
	var kept []int
	for _, c := range cands {
		a, l := across(c), along(c)
		isChar := a >= 0.55*charSize && a <= 1.45*charSize
		// Dashes and similar: thin across the line, but not specks.
		isDash := a >= 0.1*charSize && a < 0.55*charSize && l >= 0.3*charSize && l <= 1.0*charSize
		if isChar || isDash {
			keep[c.idx] = true
			kept = append(kept, c.idx)
		}
	}
	// Drop isolated components far from every other kept component.
	if len(kept) > 2 {
		for _, i := range kept {
			near := false
			for _, j := range kept {
				if i != j && rectGap(comps[i], comps[j]) < 2.0*charSize {
					near = true
					break
				}
			}
			if !near {
				keep[i] = false
			}
		}
	}

	cons := sh.conc
	if vertical {
		cons = sw.conc
	}
	out := &TextLayout{Mask: NewMask(w, h), CharSize: charSize, Vertical: vertical, Consistency: cons}
	for i, c := range comps {
		if !keep[i] {
			continue
		}
		out.Count++
		out.Bounds = out.Bounds.Union(image.Rect(c.MinX, c.MinY, c.MaxX+1, c.MaxY+1))
	}
	if out.Count == 0 {
		return nil
	}
	for i, l := range labels {
		if l != 0 && keep[l-1] {
			out.Mask.Pix[i] = true
		}
	}
	return out
}

// textThreshold picks the top-hat level separating marking strokes from
// the body. Otsu alone fails when bright non-text structures (package edge
// highlights, a lead that survived masking) dominate the upper tail, so it
// is capped by a robust noise-based level: median + k * MAD of the body.
func textThreshold(tophat *Plane, body *Mask) float32 {
	vals := maskedValues(tophat, body)
	if len(vals) == 0 {
		return otsu(tophat)
	}
	t := otsu(&Plane{W: len(vals), H: 1, Pix: vals})
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	med := vals[len(vals)/2]
	dev := make([]float32, len(vals))
	for i, v := range vals {
		d := v - med
		if d < 0 {
			d = -d
		}
		dev[i] = d
	}
	sort.Slice(dev, func(i, j int) bool { return dev[i] < dev[j] })
	mad := dev[len(dev)/2]
	if noise := med + 6*mad; noise < t {
		t = noise
	}
	// Never go below a minimal contrast.
	return max(t, 6)
}

// bodyEdge marks body pixels adjacent to non-body pixels.
func bodyEdge(body *Mask) *Mask {
	in := erode(body, 1)
	out := NewMask(body.W, body.H)
	for i := range out.Pix {
		out.Pix[i] = body.Pix[i] && !in.Pix[i]
	}
	return out
}

// rectGap is the Chebyshev distance between two component bounding boxes
// (0 if they overlap).
func rectGap(a, b component) float64 {
	dx := max(0, max(a.MinX-b.MaxX, b.MinX-a.MaxX))
	dy := max(0, max(a.MinY-b.MaxY, b.MinY-a.MaxY))
	return float64(max(dx, dy))
}

// resample scales p to nw x nh using area averaging when shrinking and
// bilinear interpolation when enlarging.
func resample(p *Plane, nw, nh int) *Plane {
	nw, nh = max(1, nw), max(1, nh)
	sx := float64(p.W) / float64(nw)
	sy := float64(p.H) / float64(nh)
	src := p
	if sx > 1 || sy > 1 {
		// Pre-blur to approximate area averaging.
		r := int(math.Floor(math.Max(sx, sy) / 2))
		if r > 0 {
			src = boxBlur(p, r)
		}
	}
	out := NewPlane(nw, nh)
	for y := 0; y < nh; y++ {
		fy := (float64(y)+0.5)*sy - 0.5
		y0 := int(math.Floor(fy))
		ty := float32(fy - float64(y0))
		for x := 0; x < nw; x++ {
			fx := (float64(x)+0.5)*sx - 0.5
			x0 := int(math.Floor(fx))
			tx := float32(fx - float64(x0))
			a := src.At(x0, y0)*(1-tx) + src.At(x0+1, y0)*tx
			b := src.At(x0, y0+1)*(1-tx) + src.At(x0+1, y0+1)*tx
			out.Pix[y*nw+x] = a*(1-ty) + b*ty
		}
	}
	return out
}

// subPlane copies the rectangle r (clipped) out of p.
func subPlane(p *Plane, r image.Rectangle) *Plane {
	r = r.Intersect(image.Rect(0, 0, p.W, p.H))
	out := NewPlane(r.Dx(), r.Dy())
	for y := 0; y < out.H; y++ {
		copy(out.Pix[y*out.W:(y+1)*out.W], p.Pix[(r.Min.Y+y)*p.W+r.Min.X:])
	}
	return out
}
