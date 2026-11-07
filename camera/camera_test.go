package camera

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamAndCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/stream.mjpeg":
			w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=camweb-frame")
			for i := 1; i <= 3; i++ {
				body := fmt.Sprintf("frame%d", i)
				fmt.Fprintf(w, "--camweb-frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n%s\r\n", len(body), body)
			}
			fmt.Fprint(w, "--camweb-frame--\r\n")
		case r.Method == "POST" && r.URL.Path == "/api/capture":
			w.Header().Set("Content-Type", "image/jpeg")
			fmt.Fprint(w, "snapshot")
		default:
			http.Error(w, `{"error":"no"}`, http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/")

	var got []string
	err := c.Stream(context.Background(), func(b []byte) { got = append(got, string(b)) })
	if err == nil {
		t.Error("Stream returned nil error at end of stream")
	}
	if fmt.Sprint(got) != "[frame1 frame2 frame3]" {
		t.Errorf("frames = %v", got)
	}

	snap, err := c.Capture(context.Background())
	if err != nil || string(snap) != "snapshot" {
		t.Errorf("Capture = %q, %v", snap, err)
	}

	bad := New(srv.URL + "/nothing")
	if _, err := bad.Capture(context.Background()); err == nil {
		t.Error("Capture on a failing server returned no error")
	}
}

func TestLatestNext(t *testing.T) {
	var l Latest
	if f, _ := l.Get(); f.Seq != 0 {
		t.Fatalf("empty Latest has Seq %d", f.Seq)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		l.Put([]byte("a"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	f, err := l.Next(ctx, 0)
	if err != nil || string(f.JPEG) != "a" || f.Seq != 1 {
		t.Fatalf("Next = %+v, %v", f, err)
	}
	// Nothing newer than 1: Next must time out.
	short, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, err := l.Next(short, 1); err == nil {
		t.Error("Next returned without a new frame")
	}
}
