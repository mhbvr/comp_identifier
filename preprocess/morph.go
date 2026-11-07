package preprocess

// Mask is a binary image; true marks foreground.
type Mask struct {
	W, H int
	Pix  []bool
}

// NewMask allocates an empty mask.
func NewMask(w, h int) *Mask {
	return &Mask{W: w, H: h, Pix: make([]bool, w*h)}
}

// Count returns the number of foreground pixels.
func (m *Mask) Count() int {
	n := 0
	for _, v := range m.Pix {
		if v {
			n++
		}
	}
	return n
}

// threshold returns a mask of pixels strictly greater than t.
func threshold(p *Plane, t float32) *Mask {
	m := NewMask(p.W, p.H)
	for i, v := range p.Pix {
		m.Pix[i] = v > t
	}
	return m
}

// boxCount1D computes, for each position of a row/column of length n read
// through get, the number of true values within [i-r, i+r]. Out of range
// positions count as outside (false).
func boxCount1D(n, r int, get func(int) bool, out []int) {
	sum := 0
	// prime window [0, r-1]
	for i := 0; i < r && i < n; i++ {
		if get(i) {
			sum++
		}
	}
	for i := 0; i < n; i++ {
		if j := i + r; j < n && get(j) {
			sum++
		}
		if j := i - r - 1; j >= 0 && get(j) {
			sum--
		}
		out[i] = sum
	}
}

// dilate grows the mask with a (2r+1)x(2r+1) square structuring element.
func dilate(m *Mask, r int) *Mask {
	if r <= 0 {
		return &Mask{W: m.W, H: m.H, Pix: append([]bool(nil), m.Pix...)}
	}
	return squareFilter(m, r, func(cnt, _ int) bool { return cnt > 0 })
}

// erode shrinks the mask with a (2r+1)x(2r+1) square structuring element.
// Pixels outside the image are treated as background.
func erode(m *Mask, r int) *Mask {
	if r <= 0 {
		return &Mask{W: m.W, H: m.H, Pix: append([]bool(nil), m.Pix...)}
	}
	return squareFilter(m, r, func(cnt, full int) bool { return cnt == full })
}

// squareFilter runs a separable box count and applies keep to the result.
// keep receives the count in the window and the full window size along that
// axis. The horizontal and vertical passes are applied in sequence.
func squareFilter(m *Mask, r int, keep func(cnt, full int) bool) *Mask {
	w, h := m.W, m.H
	full := 2*r + 1
	tmp := NewMask(w, h)
	row := make([]int, w)
	for y := 0; y < h; y++ {
		off := y * w
		boxCount1D(w, r, func(i int) bool { return m.Pix[off+i] }, row)
		for x := 0; x < w; x++ {
			tmp.Pix[off+x] = keep(row[x], full)
		}
	}
	out := NewMask(w, h)
	col := make([]int, h)
	for x := 0; x < w; x++ {
		boxCount1D(h, r, func(i int) bool { return tmp.Pix[i*w+x] }, col)
		for y := 0; y < h; y++ {
			out.Pix[y*w+x] = keep(col[y], full)
		}
	}
	return out
}

func open(m *Mask, r int) *Mask   { return dilate(erode(m, r), r) }
func closeM(m *Mask, r int) *Mask { return erode(dilate(m, r), r) }

// component describes a connected region of a mask.
type component struct {
	Label int
	Area  int
	MinX  int
	MinY  int
	MaxX  int
	MaxY  int
	SumX  float64
	SumY  float64
}

// components labels 8-connected foreground regions. labels[i] is 0 for
// background and k+1 for the k-th returned component.
func components(m *Mask) ([]int32, []component) {
	w, h := m.W, m.H
	labels := make([]int32, w*h)
	var comps []component
	stack := make([]int, 0, 1024)
	for start, v := range m.Pix {
		if !v || labels[start] != 0 {
			continue
		}
		lbl := int32(len(comps) + 1)
		c := component{Label: int(lbl), MinX: w, MinY: h, MaxX: -1, MaxY: -1}
		labels[start] = lbl
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := i%w, i/w
			c.Area++
			c.SumX += float64(x)
			c.SumY += float64(y)
			if x < c.MinX {
				c.MinX = x
			}
			if x > c.MaxX {
				c.MaxX = x
			}
			if y < c.MinY {
				c.MinY = y
			}
			if y > c.MaxY {
				c.MaxY = y
			}
			for dy := -1; dy <= 1; dy++ {
				ny := y + dy
				if ny < 0 || ny >= h {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					nx := x + dx
					if nx < 0 || nx >= w {
						continue
					}
					j := ny*w + nx
					if m.Pix[j] && labels[j] == 0 {
						labels[j] = lbl
						stack = append(stack, j)
					}
				}
			}
		}
		comps = append(comps, c)
	}
	return labels, comps
}

// keepLabel returns a mask containing only pixels with the given label.
func keepLabel(labels []int32, w, h int, lbl int) *Mask {
	out := NewMask(w, h)
	l := int32(lbl)
	for i, v := range labels {
		out.Pix[i] = v == l
	}
	return out
}

// maxFilter is a grayscale dilation with a (2r+1)x(2r+1) square, using
// separable monotonic-deque sliding windows.
func maxFilter(p *Plane, r int) *Plane {
	return slidingExtreme(p, r, func(a, b float32) bool { return a >= b })
}

func slidingExtreme(p *Plane, r int, better func(a, b float32) bool) *Plane {
	if r <= 0 {
		return p.Clone()
	}
	w, h := p.W, p.H
	tmp := NewPlane(w, h)
	line := func(n int, get func(int) float32, set func(int, float32)) {
		dq := make([]int, 0, 2*r+2)
		// Window for output i is [i-r, i+r] clamped to [0,n-1].
		next := 0
		for i := 0; i < n; i++ {
			for ; next <= i+r && next < n; next++ {
				v := get(next)
				for len(dq) > 0 && better(v, get(dq[len(dq)-1])) {
					dq = dq[:len(dq)-1]
				}
				dq = append(dq, next)
			}
			for dq[0] < i-r {
				dq = dq[1:]
			}
			set(i, get(dq[0]))
		}
	}
	for y := 0; y < h; y++ {
		off := y * w
		line(w, func(i int) float32 { return p.Pix[off+i] }, func(i int, v float32) { tmp.Pix[off+i] = v })
	}
	out := NewPlane(w, h)
	for x := 0; x < w; x++ {
		xx := x
		line(h, func(i int) float32 { return tmp.Pix[i*w+xx] }, func(i int, v float32) { out.Pix[i*w+xx] = v })
	}
	return out
}

// boxBlur applies a separable mean filter with radius r (edges clamped).
func boxBlur(p *Plane, r int) *Plane {
	if r <= 0 {
		return p.Clone()
	}
	w, h := p.W, p.H
	tmp := NewPlane(w, h)
	inv := 1 / float32(2*r+1)
	for y := 0; y < h; y++ {
		var s float32
		for i := -r; i <= r; i++ {
			s += p.At(i, y)
		}
		for x := 0; x < w; x++ {
			tmp.Pix[y*w+x] = s * inv
			s += p.At(x+r+1, y) - p.At(x-r, y)
		}
	}
	out := NewPlane(w, h)
	for x := 0; x < w; x++ {
		var s float32
		for i := -r; i <= r; i++ {
			s += tmp.At(x, i)
		}
		for y := 0; y < h; y++ {
			out.Pix[y*w+x] = s * inv
			s += tmp.At(x, y+r+1) - tmp.At(x, y-r)
		}
	}
	return out
}
