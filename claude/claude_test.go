package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// fakeAPI answers POST /v1/messages with reply and records the request.
func fakeAPI(t *testing.T, reply string) (*httptest.Server, *map[string]any, *http.Header) {
	t.Helper()
	var req map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &req, &hdr
}

const okReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
 "content":[{"type":"text","text":"{\"marking\":\"TL3542 / 46M\",\"part_number\":\"TLV3542IDR\",\"manufacturer\":\"Texas Instruments\",\"description\":\"dual op amp, SOIC-8\",\"datasheet_url\":\"https://www.ti.com/lit/ds/symlink/tlv3542.pdf\",\"confidence\":\"high\"}"}],
 "stop_reason":"end_turn","usage":{"input_tokens":512,"output_tokens":140}}`

func TestIdentify(t *testing.T) {
	srv, req, hdr := fakeAPI(t, okReply)
	c := New("", option.WithBaseURL(srv.URL), option.WithAPIKey("test"))
	img := image.NewRGBA(image.Rect(0, 0, 1000, 360)) // a long DIP crop
	id, err := c.Identify(context.Background(), img, "TL3S42 / 46M")
	if err != nil {
		t.Fatal(err)
	}
	if id.PartNumber != "TLV3542IDR" || id.Confidence != "high" || id.InputTokens != 512 || id.OutputTokens != 140 || id.Model != "claude-opus-5-5" {
		t.Errorf("identification = %+v", id)
	}

	r := *req
	if r["model"] != DefaultModel || r["fallbacks"] != "default" {
		t.Errorf("model %v, fallbacks %v", r["model"], r["fallbacks"])
	}
	oc := r["output_config"].(map[string]any)
	if oc["effort"] != "low" || oc["format"].(map[string]any)["type"] != "json_schema" {
		t.Errorf("output_config = %v", oc)
	}
	if !strings.Contains(hdr.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("beta header = %q", hdr.Get("Anthropic-Beta"))
	}
	// The image is a JPEG scaled to fit 512x384, followed by the OCR hint.
	content := r["messages"].([]any)[0].(map[string]any)["content"].([]any)
	src := content[0].(map[string]any)["source"].(map[string]any)
	data, _ := base64.StdEncoding.DecodeString(src["data"].(string))
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || src["media_type"] != "image/jpeg" || cfg.Width != 512 || cfg.Height != 184 {
		t.Errorf("image %v %dx%d, err %v", src["media_type"], cfg.Width, cfg.Height, err)
	}
	if hint := content[1].(map[string]any)["text"]; hint != "OCR hint: TL3S42 / 46M" {
		t.Errorf("hint = %q", hint)
	}
}

func TestIdentifyRefusal(t *testing.T) {
	srv, _, _ := fakeAPI(t, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],
	 "stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":""},"usage":{"input_tokens":1,"output_tokens":1}}`)
	c := New("", option.WithBaseURL(srv.URL), option.WithAPIKey("test"))
	if _, err := c.Identify(context.Background(), image.NewRGBA(image.Rect(0, 0, 10, 10)), ""); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("err = %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	// 1000x360 shrinks to 512x184: 126 image tokens plus the prompt.
	if got := EstimateTokens(1000, 360); got != promptTokens+126 {
		t.Errorf("EstimateTokens(1000, 360) = %d", got)
	}
	// Small images are not enlarged.
	if got := EstimateTokens(300, 250); got != promptTokens+100 {
		t.Errorf("EstimateTokens(300, 250) = %d", got)
	}
}
