// Command compweb is a web app that identifies electronic components under
// a camera served by camera_rpi (https://github.com/mhbvr/camera_rpi).
//
//	compweb -camera http://raspberrypi.local:8080 -listen :8090
//
// Open http://localhost:8090: the live view shows a rectangle around the
// detected component (orange while recognising, green for a confident
// reading, red for a doubtful one), next to the recognised text and the
// images it was read from.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/mhbvr/comp_identifier/camera"
	"github.com/mhbvr/comp_identifier/marking"
	"github.com/mhbvr/comp_identifier/ocr"
	"github.com/mhbvr/comp_identifier/tracker"
	"github.com/mhbvr/comp_identifier/web"
)

func main() {
	ocr.LimitOpenMPThreads()

	listen := flag.String("listen", ":8090", "HTTP listen address")
	camURL := flag.String("camera", "http://raspberrypi.local:8080", "camera_rpi (camweb) base URL")
	camCA := flag.String("camera-ca", "", "PEM CA certificate to trust for an https:// camera")
	useCapture := flag.Bool("capture", true, "recognise full-resolution captures (POST /api/capture); false: use stream frames")
	tessdata := flag.String("tessdata", "", "Tesseract tessdata directory (default: system models)")
	flag.Parse()

	cam := camera.New(*camURL)
	if *camCA != "" {
		client, err := httpClientWithCA(*camCA)
		if err != nil {
			log.Fatal(err)
		}
		cam.HTTP = client
	}

	opt := marking.DefaultOptions()
	opt.OCR.TessdataPrefix = *tessdata
	reader, err := marking.NewReader(opt)
	if err != nil {
		log.Fatal(err)
	}
	defer reader.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	frames := &camera.Latest{}
	go cam.Feed(ctx, frames, log.Default())

	source := func() ([]byte, uint64, error) {
		f, err := frames.Get()
		return f.JPEG, f.Seq, err
	}
	var capture tracker.Capture
	if *useCapture {
		capture = cam.Capture
	}
	t := tracker.New(tracker.Config{}, source, capture, reader.Read)
	go t.Run(ctx)

	srv := &http.Server{Addr: *listen, Handler: web.New(t, frames)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("camera %s, serving on %s", *camURL, *listen)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// httpClientWithCA returns an HTTP client that trusts the CA in caFile (as
// made by camera_rpi's `make cert`) in addition to the system roots.
func httpClientWithCA(caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no PEM certificates", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	return &http.Client{Transport: tr}, nil
}
