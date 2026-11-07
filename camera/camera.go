// Package camera is a small client for the camera_rpi (camweb) REST API
// (https://github.com/mhbvr/camera_rpi/blob/main/docs/REST_API.md).
//
// It only reads from the camera: the MJPEG stream (GET /stream.mjpeg) and
// full-resolution snapshots (POST /api/capture). Camera settings are never
// changed.
package camera

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client talks to one camweb server.
type Client struct {
	// BaseURL is the camweb address, e.g. "http://raspberrypi.local:8080".
	BaseURL string
	// HTTP is the client used for requests. It must not have a Timeout,
	// which would cut the endless MJPEG stream; requests are bounded by
	// their context instead.
	HTTP *http.Client
}

// New returns a client for baseURL using http.DefaultClient.
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: http.DefaultClient}
}

// Stream reads GET /stream.mjpeg and calls fn with every JPEG frame until
// ctx is cancelled or the stream fails. fn may keep the slice; it is not
// reused.
func (c *Client) Stream(ctx context.Context, fn func(jpeg []byte)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/stream.mjpeg", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("camera: GET /stream.mjpeg: %s", resp.Status)
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return fmt.Errorf("camera: /stream.mjpeg is not multipart: %q", resp.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err != nil {
			return fmt.Errorf("camera: reading stream: %w", err)
		}
		jpeg, err := io.ReadAll(part)
		if err != nil {
			return fmt.Errorf("camera: reading frame: %w", err)
		}
		fn(jpeg)
	}
}

// Capture takes a full-resolution snapshot (POST /api/capture with the
// camera's configured JPEG quality) and returns the JPEG.
func (c *Client) Capture(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/capture", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("camera: POST /api/capture: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return body, nil
}

// Feed keeps l filled with the newest stream frame, reconnecting with a
// growing delay (up to 10 s) whenever the stream fails. It returns when ctx
// is cancelled. Stream errors are reported through l.Err.
func (c *Client) Feed(ctx context.Context, l *Latest, logger *log.Logger) {
	delay := time.Second
	for ctx.Err() == nil {
		err := c.Stream(ctx, func(jpeg []byte) {
			l.Put(jpeg)
			delay = time.Second
		})
		if ctx.Err() != nil {
			return
		}
		l.SetErr(err)
		logger.Printf("camera stream: %v (retrying in %v)", err, delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, 10*time.Second)
	}
}

// Frame is one JPEG from the stream.
type Frame struct {
	JPEG []byte
	Seq  uint64 // increases by one per frame; 0 means "no frame yet"
}

// Latest holds the newest frame and lets any number of readers wait for
// the next one. The zero value is ready to use.
type Latest struct {
	mu      sync.Mutex
	frame   Frame
	err     error
	changed chan struct{} // closed and replaced on every Put
}

// Put stores a new frame and wakes all waiting readers.
func (l *Latest) Put(jpeg []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frame = Frame{JPEG: jpeg, Seq: l.frame.Seq + 1}
	l.err = nil
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

// SetErr records the latest stream error (cleared by the next Put).
func (l *Latest) SetErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

// Get returns the newest frame (Seq 0 if none) and the stream error, if
// the stream is currently down.
func (l *Latest) Get() (Frame, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frame, l.err
}

// Next waits for a frame newer than seq.
func (l *Latest) Next(ctx context.Context, seq uint64) (Frame, error) {
	for {
		l.mu.Lock()
		if l.frame.Seq > seq {
			f := l.frame
			l.mu.Unlock()
			return f, nil
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		ch := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		case <-ch:
		}
	}
}
