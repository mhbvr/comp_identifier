package ocr

import (
	"image"
	"image/draw"
	"math"
)

func toRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
		return r
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Rect, img, b.Min, draw.Src)
	return out
}

// resize scales src to w x h with bilinear interpolation (pixel-centre
// aligned, like OpenCV's INTER_LINEAR, which the models were trained with).
func resize(src *image.RGBA, w, h int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	sx, sy := float64(sw)/float64(w), float64(sh)/float64(h)
	for y := 0; y < h; y++ {
		fy := math.Max(0, (float64(y)+0.5)*sy-0.5)
		y0 := min(int(fy), sh-1)
		y1 := min(y0+1, sh-1)
		ty := fy - float64(y0)
		for x := 0; x < w; x++ {
			fx := math.Max(0, (float64(x)+0.5)*sx-0.5)
			x0 := min(int(fx), sw-1)
			x1 := min(x0+1, sw-1)
			tx := fx - float64(x0)
			o := out.PixOffset(x, y)
			for c := 0; c < 3; c++ {
				a := float64(src.Pix[src.PixOffset(x0, y0)+c])*(1-tx) + float64(src.Pix[src.PixOffset(x1, y0)+c])*tx
				b := float64(src.Pix[src.PixOffset(x0, y1)+c])*(1-tx) + float64(src.Pix[src.PixOffset(x1, y1)+c])*tx
				out.Pix[o+c] = uint8(a*(1-ty) + b*ty + 0.5)
			}
			out.Pix[o+3] = 255
		}
	}
	return out
}

// tensorCHW writes img into a 3 x h x w float tensor of width tw >= w
// (the rest stays 0), channels in BGR order like the OpenCV images the
// models were trained on, each value mapped to (v/255 - mean[c]) / std[c].
func tensorCHW(img *image.RGBA, tw int, mean, std [3]float32) []float32 {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	data := make([]float32, 3*h*tw)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := img.PixOffset(x, y)
			for c := 0; c < 3; c++ {
				v := float32(img.Pix[o+2-c]) / 255 // c=0 is blue
				data[(c*h+y)*tw+x] = (v - mean[c]) / std[c]
			}
		}
	}
	return data
}
