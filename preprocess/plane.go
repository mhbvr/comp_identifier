package preprocess

import (
	"image"
	"math"
)

// Plane is a single-channel float image with values nominally in [0,255].
type Plane struct {
	W, H int
	Pix  []float32
}

// NewPlane allocates a zeroed plane.
func NewPlane(w, h int) *Plane {
	return &Plane{W: w, H: h, Pix: make([]float32, w*h)}
}

// At returns the value at (x,y) with coordinates clamped to the plane.
func (p *Plane) At(x, y int) float32 {
	if x < 0 {
		x = 0
	} else if x >= p.W {
		x = p.W - 1
	}
	if y < 0 {
		y = 0
	} else if y >= p.H {
		y = p.H - 1
	}
	return p.Pix[y*p.W+x]
}

// Clone returns a deep copy.
func (p *Plane) Clone() *Plane {
	q := NewPlane(p.W, p.H)
	copy(q.Pix, p.Pix)
	return q
}

func clamp8(v float32) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// rgbPlanes holds an RGB image split into float planes.
type rgbPlanes struct {
	R, G, B *Plane
}

// downscaleRGB converts img into float RGB planes, shrinking it by an integer
// box filter so that its longest side is at most maxSide. It returns the
// planes and the factor by which coordinates must be multiplied to map back
// into the source image.
func downscaleRGB(img *image.RGBA, maxSide int) (rgbPlanes, float64) {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	f := 1
	if maxSide > 0 {
		longest := sw
		if sh > longest {
			longest = sh
		}
		f = int(math.Ceil(float64(longest) / float64(maxSide)))
		if f < 1 {
			f = 1
		}
	}
	w, h := sw/f, sh/f
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	out := rgbPlanes{NewPlane(w, h), NewPlane(w, h), NewPlane(w, h)}
	inv := 1 / float32(f*f)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sr, sg, sb uint32
			for dy := 0; dy < f; dy++ {
				for dx := 0; dx < f; dx++ {
					o := img.PixOffset(b.Min.X+x*f+dx, b.Min.Y+y*f+dy)
					sr += uint32(img.Pix[o])
					sg += uint32(img.Pix[o+1])
					sb += uint32(img.Pix[o+2])
				}
			}
			i := y*w + x
			out.R.Pix[i] = float32(sr) * inv
			out.G.Pix[i] = float32(sg) * inv
			out.B.Pix[i] = float32(sb) * inv
		}
	}
	return out, float64(f)
}

// otsu computes Otsu's threshold for values in [0,255].
func otsu(p *Plane) float32 {
	var hist [256]float64
	for _, v := range p.Pix {
		hist[clamp8(v)]++
	}
	total := float64(len(p.Pix))
	var sum float64
	for i, c := range hist {
		sum += float64(i) * c
	}
	var sumB, wB, best float64
	thr := 0
	for t := 0; t < 256; t++ {
		wB += hist[t]
		if wB == 0 {
			continue
		}
		wF := total - wB
		if wF == 0 {
			break
		}
		sumB += float64(t) * hist[t]
		mB := sumB / wB
		mF := (sum - sumB) / wF
		between := wB * wF * (mB - mF) * (mB - mF)
		if between > best {
			best = between
			thr = t
		}
	}
	return float32(thr) + 0.5
}
