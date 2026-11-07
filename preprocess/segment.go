package preprocess

import (
	"errors"
	"image"
	"image/draw"
	"math"
)

// ErrNoComponent is returned when no foreground object could be found.
var ErrNoComponent = errors.New("preprocess: no component found in image")

// Segmentation is the result of locating the component on the background.
type Segmentation struct {
	// Rect is the minimum-area rectangle around the component body in
	// source image coordinates, normalised so that W >= H.
	Rect RotatedRect
	// Area is the body's share of the image area, in [0,1].
	Area float64
	// Mask is the final body mask at working resolution (debug aid).
	Mask *Mask
	// Foreground is the background-normalised "darkness" map used for
	// thresholding, at working resolution (debug aid).
	Foreground *Plane
	// Scale maps working-resolution coordinates to source coordinates.
	Scale float64
	// Threshold is the Otsu threshold applied to Foreground.
	Threshold float32
}

// toRGBA returns img as *image.RGBA with bounds starting at (0,0).
func toRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
		return r
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Rect, img, b.Min, draw.Src)
	return out
}

// foregroundMap computes, for each pixel, how much darker / more saturated
// it is than the local white background, scaled to [0,255].
//
// The darkness measure is based on min(R,G,B): it is high for white paper,
// low for black epoxy and also low for coloured (e.g. tan or green) bodies,
// while bright metal leads stay high. The local white level is estimated
// with a large max filter followed by a blur, which removes vignetting and
// colour casts from uneven lighting.
func foregroundMap(rgb rgbPlanes) *Plane {
	w, h := rgb.R.W, rgb.R.H
	mn := NewPlane(w, h)
	for i := range mn.Pix {
		v := rgb.R.Pix[i]
		if g := rgb.G.Pix[i]; g < v {
			v = g
		}
		if b := rgb.B.Pix[i]; b < v {
			v = b
		}
		mn.Pix[i] = v
	}
	longest := w
	if h > longest {
		longest = h
	}
	r := longest / 6
	bg := boxBlur(maxFilter(mn, r), r/2)
	fg := NewPlane(w, h)
	for i, v := range mn.Pix {
		b := bg.Pix[i]
		if b < 1 {
			b = 1
		}
		d := (b - v) / b * 255
		if d < 0 {
			d = 0
		}
		fg.Pix[i] = d
	}
	return fg
}

// Segment finds the component in a photo taken on a white background and
// returns its oriented bounding rectangle.
//
// Only WorkSize and MinArea of opt are used. A blob smaller than MinArea
// (a shadow, dust, a dark image corner) is not a component and yields
// ErrNoComponent, which is how an empty scene is recognised.
func Segment(img image.Image, opt Options) (*Segmentation, error) {
	opt = opt.withDefaults()
	rgb, scale := downscaleRGB(toRGBA(img), opt.WorkSize)
	fg := foregroundMap(rgb)
	t := otsu(fg)
	// Guard against nearly empty images where Otsu splits noise.
	if t < 35 {
		t = 35
	}
	mask := threshold(fg, t)

	w, h := mask.W, mask.H
	short := min(w, h)
	// Close small gaps (light marking text, glare streaks on the body).
	mask = closeM(mask, max(1, short/100))
	labels, comps := components(mask)
	best := largest(comps, 0)
	if best == nil || best.Area < 50 {
		return nil, ErrNoComponent
	}
	body := fillHoles(keepLabel(labels, w, h, best.Label))
	if core := darkCore(fg, body, short); core != nil {
		body = core
	}

	// Remove thin protrusions (leads, which may be dark when tarnished)
	// with an opening scaled to the blob size, then keep the largest part.
	r := int(math.Round(0.08 * math.Sqrt(float64(best.Area))))
	if r > 0 {
		opened := open(body, r)
		labels, comps = components(opened)
		if b := largest(comps, 0); b != nil && b.Area > best.Area/4 {
			body = keepLabel(labels, w, h, b.Label)
		}
	}

	area := float64(body.Count()) / float64(w*h)
	if area < opt.MinArea {
		return nil, ErrNoComponent
	}
	hull := convexHull(maskBoundary(body))
	rect := minAreaRect(hull).Landscape().Scale(scale)
	return &Segmentation{Rect: rect, Area: area, Mask: body, Foreground: fg, Scale: scale, Threshold: t}, nil
}

// darkCore removes a shadow merged with the body. A strong shadow can pass
// the threshold and join the body into one blob, which skews the fitted
// rectangle. It is lighter than the package, so the blob is split once more
// by Otsu over its own pixels and the dark core is kept, with gaps closed
// and holes (the marking) filled. Without a shadow the core is the whole
// body. If the core covers less than 60% of the blob (a glare-washed or
// light-coloured package) it returns nil and the blob is kept as it is.
func darkCore(fg *Plane, blob *Mask, short int) *Mask {
	t := otsuMasked(fg, blob)
	core := NewMask(blob.W, blob.H)
	for i, in := range blob.Pix {
		core.Pix[i] = in && fg.Pix[i] > t
	}
	core = closeM(core, max(1, short/100))
	labels, comps := components(core)
	best := largest(comps, 0)
	if best == nil {
		return nil
	}
	core = fillHoles(keepLabel(labels, core.W, core.H, best.Label))
	if core.Count() < blob.Count()*6/10 {
		return nil
	}
	return core
}

func largest(comps []component, minArea int) *component {
	var best *component
	for i := range comps {
		if comps[i].Area < minArea {
			continue
		}
		if best == nil || comps[i].Area > best.Area {
			best = &comps[i]
		}
	}
	return best
}

// fillHoles sets every background region not connected to the image border
// to foreground.
func fillHoles(m *Mask) *Mask {
	w, h := m.W, m.H
	outside := make([]bool, w*h)
	stack := make([]int, 0, 1024)
	push := func(x, y int) {
		i := y*w + x
		if !m.Pix[i] && !outside[i] {
			outside[i] = true
			stack = append(stack, i)
		}
	}
	for x := 0; x < w; x++ {
		push(x, 0)
		push(x, h-1)
	}
	for y := 0; y < h; y++ {
		push(0, y)
		push(w-1, y)
	}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x, y := i%w, i/w
		if x > 0 {
			push(x-1, y)
		}
		if x < w-1 {
			push(x+1, y)
		}
		if y > 0 {
			push(x, y-1)
		}
		if y < h-1 {
			push(x, y+1)
		}
	}
	out := NewMask(w, h)
	for i := range out.Pix {
		out.Pix[i] = !outside[i]
	}
	return out
}

// ToGray renders the mask as a black/white image (debug aid).
func (m *Mask) ToGray() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, m.W, m.H))
	for i, v := range m.Pix {
		if v {
			g.Pix[i] = 255
		}
	}
	return g
}
