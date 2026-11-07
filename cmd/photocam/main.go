// Command photocam imitates the two camera_rpi endpoints compweb uses,
// serving photos instead of a live camera, for development without a Pi:
//
//	photocam -listen :8080 components_photos/*.jpg
//	compweb -camera http://localhost:8080
//
// It shows each photo for -hold, then an empty white background for -gap
// (the component "removed"), then the next photo, in a loop.
//
//	GET  /stream.mjpeg  multipart/x-mixed-replace JPEG stream (-fps)
//	POST /api/capture   the current frame as a JPEG
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	hold := flag.Duration("hold", 8*time.Second, "how long each photo is shown")
	gap := flag.Duration("gap", 3*time.Second, "how long the empty background is shown between photos")
	fps := flag.Float64("fps", 8, "stream frame rate")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("usage: photocam [flags] photo.jpg...")
	}
	var photos [][]byte
	for _, p := range flag.Args() {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Fatal(err)
		}
		photos = append(photos, b)
	}
	blank := blankJPEG(2028, 1520)
	start := time.Now()

	// current returns the frame to show now.
	current := func() []byte {
		period := *hold + *gap
		n := int(time.Since(start) / period)
		if time.Since(start)%period >= *hold {
			return blank
		}
		return photos[n%len(photos)]
	}

	http.HandleFunc("GET /stream.mjpeg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=camweb-frame")
		tick := time.NewTicker(time.Duration(float64(time.Second) / *fps))
		defer tick.Stop()
		rc := http.NewResponseController(w)
		for {
			f := current()
			fmt.Fprintf(w, "--camweb-frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(f))
			w.Write(f)
			if _, err := w.Write([]byte("\r\n")); err != nil || rc.Flush() != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
		}
	})
	http.HandleFunc("POST /api/capture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(current())
	})
	log.Printf("serving %d photos on %s", len(photos), *listen)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

// blankJPEG renders white paper with mild vignetting, like the photos'
// background.
func blankJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	cx, cy := float64(w)/2, float64(h)/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := ((float64(x)-cx)*(float64(x)-cx) + (float64(y)-cy)*(float64(y)-cy)) / (cx*cx + cy*cy)
			v := uint8(248 - 25*d)
			img.SetRGBA(x, y, color.RGBA{v - 6, v, v, 255})
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	return b.Bytes()
}
