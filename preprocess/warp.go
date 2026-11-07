package preprocess

import (
	"image"
	"math"
)

// ExtractRect cuts the rotated rectangle r out of src and returns it as an
// axis-aligned image whose X axis follows r's W side. scale is the number of
// output pixels per source pixel. Bilinear interpolation is used; samples
// outside the source are filled with white.
func ExtractRect(src image.Image, r RotatedRect, scale float64) *image.RGBA {
	s := toRGBA(src)
	ow := int(math.Round(r.W * scale))
	oh := int(math.Round(r.H * scale))
	if ow < 1 {
		ow = 1
	}
	if oh < 1 {
		oh = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	ux, uy := math.Cos(r.Angle), math.Sin(r.Angle)
	vx, vy := -uy, ux
	sw, sh := s.Rect.Dx(), s.Rect.Dy()
	for oy := 0; oy < oh; oy++ {
		v := (float64(oy)+0.5)/scale - r.H/2
		for ox := 0; ox < ow; ox++ {
			u := (float64(ox)+0.5)/scale - r.W/2
			// Continuous source coordinate; pixel centres are at +0.5.
			sx := r.Center.X + u*ux + v*vx - 0.5
			sy := r.Center.Y + u*uy + v*vy - 0.5
			o := out.PixOffset(ox, oy)
			bilinear(s, sw, sh, sx, sy, out.Pix[o:o+4])
		}
	}
	return out
}

func bilinear(s *image.RGBA, w, h int, x, y float64, dst []uint8) {
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	fx := x - float64(x0)
	fy := y - float64(y0)
	var acc [3]float64
	for j := 0; j < 2; j++ {
		wy := 1 - fy
		if j == 1 {
			wy = fy
		}
		for i := 0; i < 2; i++ {
			wx := 1 - fx
			if i == 1 {
				wx = fx
			}
			px, py := x0+i, y0+j
			wgt := wx * wy
			if px < 0 || py < 0 || px >= w || py >= h {
				acc[0] += 255 * wgt
				acc[1] += 255 * wgt
				acc[2] += 255 * wgt
				continue
			}
			o := s.PixOffset(px, py)
			acc[0] += float64(s.Pix[o]) * wgt
			acc[1] += float64(s.Pix[o+1]) * wgt
			acc[2] += float64(s.Pix[o+2]) * wgt
		}
	}
	dst[0] = uint8(math.Min(255, acc[0]+0.5))
	dst[1] = uint8(math.Min(255, acc[1]+0.5))
	dst[2] = uint8(math.Min(255, acc[2]+0.5))
	dst[3] = 255
}

// RotateRGBA rotates img clockwise by quarterTurns * 90 degrees.
func RotateRGBA(img *image.RGBA, quarterTurns int) *image.RGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	out := image.NewRGBA(rotatedBounds(w, h, quarterTurns))
	rotatePix(out.Pix, out.Stride, img.Pix[img.PixOffset(img.Rect.Min.X, img.Rect.Min.Y):], img.Stride, w, h, 4, quarterTurns)
	return out
}

func rotatedBounds(w, h, quarterTurns int) image.Rectangle {
	if quarterTurns%2 != 0 {
		w, h = h, w
	}
	return image.Rect(0, 0, w, h)
}

// rotatePix copies a w x h image of bpp bytes per pixel from src to dst,
// rotated clockwise by quarterTurns * 90 degrees.
func rotatePix(dst []uint8, dstStride int, src []uint8, srcStride, w, h, bpp, quarterTurns int) {
	q := (quarterTurns%4 + 4) % 4
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			nx, ny := x, y
			switch q {
			case 1: // 90° clockwise
				nx, ny = h-1-y, x
			case 2:
				nx, ny = w-1-x, h-1-y
			case 3: // 90° counter-clockwise
				nx, ny = y, w-1-x
			}
			copy(dst[ny*dstStride+nx*bpp:][:bpp], src[y*srcStride+x*bpp:][:bpp])
		}
	}
}
