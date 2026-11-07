package preprocess

import (
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"testing"
)

func angleDiff(a, b float64) float64 {
	// Rectangle orientation is defined modulo 180 degrees.
	d := math.Mod(math.Abs(a-b), math.Pi)
	return math.Min(d, math.Pi-d)
}

func TestMinAreaRectOfRotatedRectangle(t *testing.T) {
	want := RotatedRect{Center: Point{120, 80}, W: 100, H: 40, Angle: 30 * math.Pi / 180}
	c := want.Corners()
	var pts []Point
	// Corners plus points along the edges and inside.
	for i := 0; i < 4; i++ {
		a, b := c[i], c[(i+1)%4]
		for s := 0.0; s <= 1; s += 0.1 {
			pts = append(pts, Point{a.X + (b.X-a.X)*s, a.Y + (b.Y-a.Y)*s})
		}
	}
	pts = append(pts, want.Center)
	got := minAreaRect(convexHull(pts)).Landscape()
	if math.Abs(got.W-want.W) > 1e-6 || math.Abs(got.H-want.H) > 1e-6 {
		t.Errorf("size = %.3fx%.3f, want %.0fx%.0f", got.W, got.H, want.W, want.H)
	}
	if math.Hypot(got.Center.X-want.Center.X, got.Center.Y-want.Center.Y) > 1e-6 {
		t.Errorf("center = %v, want %v", got.Center, want.Center)
	}
	if angleDiff(got.Angle, want.Angle) > 1e-6 {
		t.Errorf("angle = %.3f°, want 30°", got.Angle*180/math.Pi)
	}
}

func TestLandscape(t *testing.T) {
	r := RotatedRect{W: 10, H: 30, Angle: 0}.Landscape()
	if r.W != 30 || r.H != 10 || angleDiff(r.Angle, math.Pi/2) > 1e-9 {
		t.Errorf("got %+v", r)
	}
	if r.Angle <= -math.Pi/2 || r.Angle > math.Pi/2 {
		t.Errorf("angle %v not normalised", r.Angle)
	}
}

func TestErodeDilate(t *testing.T) {
	m := NewMask(9, 9)
	for y := 2; y <= 6; y++ {
		for x := 2; x <= 6; x++ {
			m.Pix[y*9+x] = true
		}
	}
	if n := erode(m, 1).Count(); n != 9 {
		t.Errorf("erode count = %d, want 9", n)
	}
	if n := dilate(m, 1).Count(); n != 49 {
		t.Errorf("dilate count = %d, want 49", n)
	}
	// A one-pixel line disappears under opening.
	line := NewMask(9, 9)
	for x := 0; x < 9; x++ {
		line.Pix[4*9+x] = true
	}
	if n := open(line, 1).Count(); n != 0 {
		t.Errorf("open(line) count = %d, want 0", n)
	}
}

func TestMinMaxFilter(t *testing.T) {
	p := NewPlane(5, 1)
	copy(p.Pix, []float32{5, 1, 7, 3, 9})
	mn := minFilter(p, 1)
	mx := maxFilter(p, 1)
	wantMin := []float32{1, 1, 1, 3, 3}
	wantMax := []float32{5, 7, 7, 9, 9}
	for i := range wantMin {
		if mn.Pix[i] != wantMin[i] || mx.Pix[i] != wantMax[i] {
			t.Fatalf("min=%v max=%v", mn.Pix, mx.Pix)
		}
	}
}

func TestRotateGray(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 3, 2))
	copy(g.Pix, []uint8{1, 2, 3, 4, 5, 6})
	r := RotateGray(g, 1) // 90° clockwise
	if r.Rect.Dx() != 2 || r.Rect.Dy() != 3 {
		t.Fatalf("size %v", r.Rect)
	}
	want := []uint8{4, 1, 5, 2, 6, 3}
	for i, v := range want {
		if r.Pix[i] != v {
			t.Fatalf("got %v, want %v", r.Pix, want)
		}
	}
	back := RotateGray(RotateGray(r, 2), 1)
	for i := range g.Pix {
		if back.Pix[i] != g.Pix[i] {
			t.Fatalf("four quarter turns changed the image: %v", back.Pix)
		}
	}
}

func TestRotateRGBA(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.SetRGBA(0, 0, color.RGBA{1, 2, 3, 255})
	img.SetRGBA(1, 0, color.RGBA{4, 5, 6, 255})
	r := RotateRGBA(img, 1) // 90° clockwise: left pixel ends up on top
	if r.Rect.Dx() != 1 || r.Rect.Dy() != 2 {
		t.Fatalf("size %v", r.Rect)
	}
	if r.RGBAAt(0, 0).R != 1 || r.RGBAAt(0, 1).R != 4 {
		t.Errorf("got %v %v", r.RGBAAt(0, 0), r.RGBAAt(0, 1))
	}
}

func TestPolarityString(t *testing.T) {
	if LightOnDark.String() != "light-on-dark" || DarkOnLight.String() != "dark-on-light" {
		t.Error("unexpected polarity names")
	}
}

// synthPhoto renders a dark package with light "characters" (bars) on an
// unevenly lit white background, rotated by angle.
func synthPhoto(angle float64, nChars int) (image.Image, RotatedRect) {
	const w, h = 900, 650
	rect := RotatedRect{Center: Point{460, 320}, W: 420, H: 180, Angle: angle}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	ux, uy := math.Cos(angle), math.Sin(angle)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Vignetting: background darkens towards the right.
			bg := uint8(245 - 40*x/w)
			col := color.RGBA{bg, bg, bg + 5, 255}
			dx, dy := float64(x)-rect.Center.X, float64(y)-rect.Center.Y
			u := dx*ux + dy*uy
			v := -dx*uy + dy*ux
			if math.Abs(u) < rect.W/2 && math.Abs(v) < rect.H/2 {
				col = color.RGBA{38, 38, 40, 255}
				// Characters: bars 16 wide, 60 tall, every 40 px along u.
				start := -float64(nChars-1) * 20
				for i := 0; i < nChars; i++ {
					cu := start + float64(i)*40
					if math.Abs(u-cu) < 8 && math.Abs(v) < 30 {
						col = color.RGBA{190, 185, 170, 255}
					}
				}
			}
			img.SetRGBA(x, y, col)
		}
	}
	return img, rect
}

func TestSegmentSynthetic(t *testing.T) {
	for _, deg := range []float64{0, 17, 45, -62} {
		img, want := synthPhoto(deg*math.Pi/180, 6)
		seg, err := Segment(img, Options{})
		if err != nil {
			t.Fatalf("%v°: %v", deg, err)
		}
		got := seg.Rect
		if math.Abs(got.W-want.W) > 0.04*want.W || math.Abs(got.H-want.H) > 0.06*want.H {
			t.Errorf("%v°: size %.0fx%.0f, want %.0fx%.0f", deg, got.W, got.H, want.W, want.H)
		}
		if d := angleDiff(got.Angle, want.Angle) * 180 / math.Pi; d > 2 {
			t.Errorf("%v°: angle off by %.1f°", deg, d)
		}
		if d := math.Hypot(got.Center.X-want.Center.X, got.Center.Y-want.Center.Y); d > 6 {
			t.Errorf("%v°: centre off by %.1f px", deg, d)
		}
	}
}

func TestProcessFindsCharacters(t *testing.T) {
	img, _ := synthPhoto(28*math.Pi/180, 6)
	res, err := Process(img, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Crop.Rect.Dx() < res.Crop.Rect.Dy() {
		t.Errorf("crop is not landscape: %v", res.Crop.Rect)
	}
	if res.Polarity != LightOnDark {
		t.Errorf("polarity = %v", res.Polarity)
	}
	if res.Text == nil {
		t.Fatal("no text layout found")
	}
	if res.Text.Count != 6 {
		t.Errorf("found %d characters, want 6", res.Text.Count)
	}
	if res.Text.Vertical {
		t.Error("text reported vertical, want horizontal")
	}
	names := map[string]bool{}
	for _, v := range res.Variants {
		names[v.Name] = true
		if v.Image.Rect.Empty() {
			t.Errorf("variant %s is empty", v.Name)
		}
	}
	for _, n := range []string{"clean", "soft", "tophat"} {
		if !names[n] {
			t.Errorf("missing variant %q", n)
		}
	}
}

func TestSegmentEmptyImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 200, 150))
	for i := range img.Pix {
		img.Pix[i] = 250
	}
	if _, err := Segment(img, Options{}); err != ErrNoComponent {
		t.Errorf("err = %v, want ErrNoComponent", err)
	}
}

func TestSegmentVignettedBlank(t *testing.T) {
	// Strongly vignetted, slightly tinted paper with no component.
	img := image.NewRGBA(image.Rect(0, 0, 1440, 1080))
	for y := 0; y < 1080; y++ {
		for x := 0; x < 1440; x++ {
			d := float64((x-720)*(x-720)+(y-540)*(y-540)) / (900 * 900)
			v := uint8(245 - 50*d)
			img.SetRGBA(x, y, color.RGBA{v - 15, v, v, 255})
		}
	}
	if _, err := Segment(img, Options{}); err != ErrNoComponent {
		t.Errorf("err = %v, want ErrNoComponent", err)
	}
}

// TestSegmentIgnoresShadow uses a real photo where a strong shadow on the
// paper touches the chip. The rectangle must follow the chip (tilted about
// -17°), not the chip and shadow together (which gave 0° and a much wider
// box).
func TestSegmentIgnoresShadow(t *testing.T) {
	f, err := os.Open("testdata/shadow_tlc3702.jpg")
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	seg, err := Segment(img, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := seg.Rect
	if deg := r.Angle * 180 / math.Pi; deg > -14 || deg < -21 {
		t.Errorf("angle = %.1f°, want about -17°", deg)
	}
	if r.W > 360 || r.Center.X < 360 {
		t.Errorf("rectangle %+v still includes the shadow on the left", r)
	}
}
