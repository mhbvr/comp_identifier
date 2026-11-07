package preprocess

import (
	"math"
	"sort"
)

// Point is a 2D point in image coordinates (x right, y down).
type Point struct{ X, Y float64 }

// RotatedRect is a rectangle of size W x H centred at Center whose W side is
// oriented along Angle (radians, measured from the +X axis towards +Y).
type RotatedRect struct {
	Center Point
	W, H   float64
	Angle  float64
}

// Corners returns the four corners in order: top-left, top-right,
// bottom-right, bottom-left in the rectangle's own frame.
func (r RotatedRect) Corners() [4]Point {
	ux, uy := math.Cos(r.Angle), math.Sin(r.Angle) // along W
	vx, vy := -uy, ux                              // along H
	hw, hh := r.W/2, r.H/2
	c := r.Center
	return [4]Point{
		{c.X - ux*hw - vx*hh, c.Y - uy*hw - vy*hh},
		{c.X + ux*hw - vx*hh, c.Y + uy*hw - vy*hh},
		{c.X + ux*hw + vx*hh, c.Y + uy*hw + vy*hh},
		{c.X - ux*hw + vx*hh, c.Y - uy*hw + vy*hh},
	}
}

// Scale returns the rectangle with all coordinates multiplied by k.
func (r RotatedRect) Scale(k float64) RotatedRect {
	return RotatedRect{Center: Point{r.Center.X * k, r.Center.Y * k}, W: r.W * k, H: r.H * k, Angle: r.Angle}
}

// Grow returns the rectangle enlarged by the given fraction on every side.
func (r RotatedRect) Grow(frac float64) RotatedRect {
	m := frac * math.Min(r.W, r.H)
	r.W += 2 * m
	r.H += 2 * m
	return r
}

// Landscape returns an equivalent rectangle with W >= H and the angle
// normalised into (-pi/2, pi/2].
func (r RotatedRect) Landscape() RotatedRect {
	if r.H > r.W {
		r.W, r.H = r.H, r.W
		r.Angle += math.Pi / 2
	}
	for r.Angle > math.Pi/2 {
		r.Angle -= math.Pi
	}
	for r.Angle <= -math.Pi/2 {
		r.Angle += math.Pi
	}
	return r
}

func cross(o, a, b Point) float64 {
	return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X)
}

// convexHull returns the convex hull of pts in counter-clockwise order
// (Andrew's monotone chain). Collinear points are dropped.
func convexHull(pts []Point) []Point {
	if len(pts) < 3 {
		return append([]Point(nil), pts...)
	}
	p := append([]Point(nil), pts...)
	sort.Slice(p, func(i, j int) bool {
		if p[i].X != p[j].X {
			return p[i].X < p[j].X
		}
		return p[i].Y < p[j].Y
	})
	hull := make([]Point, 0, 2*len(p))
	for _, pt := range p {
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], pt) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, pt)
	}
	lower := len(hull) + 1
	for i := len(p) - 2; i >= 0; i-- {
		pt := p[i]
		for len(hull) >= lower && cross(hull[len(hull)-2], hull[len(hull)-1], pt) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, pt)
	}
	return hull[:len(hull)-1]
}

// minAreaRect returns the minimum-area enclosing rectangle of a convex
// polygon. One side of the optimal rectangle is always collinear with a hull
// edge, so every edge direction is tried.
func minAreaRect(hull []Point) RotatedRect {
	switch len(hull) {
	case 0:
		return RotatedRect{}
	case 1:
		return RotatedRect{Center: hull[0]}
	}
	best := RotatedRect{}
	bestArea := math.Inf(1)
	n := len(hull)
	for i := 0; i < n; i++ {
		a, b := hull[i], hull[(i+1)%n]
		dx, dy := b.X-a.X, b.Y-a.Y
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		ux, uy := dx/l, dy/l
		vx, vy := -uy, ux
		minU, maxU := math.Inf(1), math.Inf(-1)
		minV, maxV := math.Inf(1), math.Inf(-1)
		for _, p := range hull {
			u := p.X*ux + p.Y*uy
			v := p.X*vx + p.Y*vy
			minU = math.Min(minU, u)
			maxU = math.Max(maxU, u)
			minV = math.Min(minV, v)
			maxV = math.Max(maxV, v)
		}
		area := (maxU - minU) * (maxV - minV)
		if area < bestArea {
			bestArea = area
			cu, cv := (minU+maxU)/2, (minV+maxV)/2
			best = RotatedRect{
				Center: Point{cu*ux + cv*vx, cu*uy + cv*vy},
				W:      maxU - minU,
				H:      maxV - minV,
				Angle:  math.Atan2(uy, ux),
			}
		}
	}
	return best
}

// maskBoundary returns the outer corner points of every foreground pixel
// that touches the background. Using pixel corners (rather than centres)
// makes the hull enclose the full pixel area.
func maskBoundary(m *Mask) []Point {
	var pts []Point
	w, h := m.W, m.H
	get := func(x, y int) bool {
		if x < 0 || y < 0 || x >= w || y >= h {
			return false
		}
		return m.Pix[y*w+x]
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !m.Pix[y*w+x] {
				continue
			}
			if get(x-1, y) && get(x+1, y) && get(x, y-1) && get(x, y+1) {
				continue
			}
			fx, fy := float64(x), float64(y)
			pts = append(pts, Point{fx, fy}, Point{fx + 1, fy}, Point{fx, fy + 1}, Point{fx + 1, fy + 1})
		}
	}
	return pts
}

// MinAreaRect returns the minimum-area rectangle enclosing pts.
func MinAreaRect(pts []Point) RotatedRect { return minAreaRect(convexHull(pts)) }
