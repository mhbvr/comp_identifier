// Package claude identifies a component with the Claude API: from a small
// photo of the package and the local OCR text it gets the marking, the
// canonical part number, the manufacturer, a short description and, when
// Claude knows it, the datasheet URL.
//
// Each request is kept small, about 500-700 input tokens:
//   - the image is the upright colour crop scaled to fit 512x384 pixels
//     (Claude bills an image as ceil(w/28) x ceil(h/28) visual tokens, so at
//     most 266 tokens), sent as JPEG;
//   - a short system prompt and the OCR text as a one-line hint;
//   - the answer is schema-constrained JSON (no prose), at low effort.
//
// No web search is used: search results add thousands of input tokens per
// request. The datasheet URL therefore comes from Claude's knowledge and is
// left empty when Claude isn't sure.
//
// Identify can also take text the user read on the part themselves, for
// markings the camera cannot see (the side of a cylindrical capacitor, the
// underside). Describe identifies a part from its part number alone, with
// no image, for components entered by hand.
package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// DefaultModel is the model used unless another one is configured.
const DefaultModel = "claude-opus-5-5"

// Image size limits: large enough for markings to stay legible, small
// enough to keep the image at or under 266 tokens.
const maxW, maxH = 512, 384

// promptTokens estimates the request's text: system prompt, JSON schema
// and OCR hint.
const promptTokens = 350

const systemPrompt = `You identify electronic components (ICs, transistors, regulators, ...) from a photo of the top of the package.
Read the marking, then give the canonical manufacturer part number (without date, lot or plant codes), the manufacturer, and a short description including the package.
Give datasheet_url only if you are sure of the manufacturer's datasheet or product page URL; otherwise leave it empty.
The OCR hint comes from a local OCR engine and may contain errors.
The user may add text they read on the part themselves, for example from the side of a cylindrical package or a face the camera cannot see. Trust it over the photo and the OCR hint, and include it in marking.`

const describePrompt = `You describe an electronic component (IC, transistor, regulator, capacitor, ...) from the part number the user typed, which may lack a suffix or have minor typos.
Give the canonical manufacturer part number, the manufacturer, and a short description, including the package when the part number tells it.
Give datasheet_url only if you are sure of the manufacturer's datasheet or product page URL; otherwise leave it empty.
If you don't recognise the part, say so in the description and set confidence to low.`

// partFields are the answer fields about the part; every field is required.
var partFields = map[string]any{
	"part_number":   map[string]any{"type": "string", "description": "canonical part number, e.g. TLC555IP"},
	"manufacturer":  map[string]any{"type": "string"},
	"description":   map[string]any{"type": "string", "description": "e.g. 'CMOS 555 timer, DIP-8'"},
	"datasheet_url": map[string]any{"type": "string"},
	"confidence":    map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
}

// schema constrains the answer of Identify, describeSchema that of Describe.
var (
	schema         = objectSchema(partFields, "marking", map[string]any{"type": "string", "description": "text printed on the part, lines separated by ' / '"})
	describeSchema = objectSchema(partFields)
)

// objectSchema is a JSON schema of an object with the given fields, all
// required, plus extra name/schema pairs.
func objectSchema(fields map[string]any, extra ...any) map[string]any {
	props := map[string]any{}
	for k, v := range fields {
		props[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		props[extra[i].(string)] = extra[i+1]
	}
	required := make([]string, 0, len(props))
	for k := range props {
		required = append(required, k)
	}
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// Identification is Claude's answer plus the request's token usage.
type Identification struct {
	Marking      string `json:"marking"`
	PartNumber   string `json:"part_number"`
	Manufacturer string `json:"manufacturer"`
	Description  string `json:"description"`
	DatasheetURL string `json:"datasheet_url"`
	Confidence   string `json:"confidence"`

	Model        string // model that answered (a fallback model after a refusal)
	InputTokens  int64
	OutputTokens int64 // includes thinking
}

// Identifier sends identification requests. It is safe for concurrent use.
type Identifier struct {
	client anthropic.Client
	model  string
}

// New creates an identifier. Credentials come from the environment
// (ANTHROPIC_API_KEY, or a profile from `ant auth login`) unless opts say
// otherwise.
func New(model string, opts ...option.RequestOption) *Identifier {
	if model == "" {
		model = DefaultModel
	}
	return &Identifier{client: anthropic.NewClient(opts...), model: model}
}

// Model returns the configured model.
func (c *Identifier) Model() string { return c.model }

// EstimateTokens returns the approximate input tokens of a request for an
// image of w x h pixels.
func EstimateTokens(w, h int) int {
	w, h = fit(w, h)
	return promptTokens + ((w+27)/28)*((h+27)/28) // 28x28-pixel visual tokens
}

// Identify asks Claude what the component in img is. ocr is the local OCR
// reading, sent as a hint. user is text the user read on the part
// themselves ("" for none), which Claude trusts over the photo.
func (c *Identifier) Identify(ctx context.Context, img image.Image, ocr, user string) (*Identification, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, shrink(img), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	content := []anthropic.BetaContentBlockParamUnion{
		anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
			Data:      base64.StdEncoding.EncodeToString(buf.Bytes()),
			MediaType: anthropic.BetaBase64ImageSourceMediaTypeImageJPEG,
		}),
		anthropic.NewBetaTextBlock("OCR hint: " + ocr),
	}
	if user = strings.TrimSpace(user); user != "" {
		content = append(content, anthropic.NewBetaTextBlock("Text the user read on the part: "+user))
	}
	return c.ask(ctx, systemPrompt, schema, content)
}

// Describe asks Claude what the component with part number name is, with
// no photo. The answer's Marking is empty.
func (c *Identifier) Describe(ctx context.Context, name string) (*Identification, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("no part number to describe")
	}
	return c.ask(ctx, describePrompt, describeSchema, []anthropic.BetaContentBlockParamUnion{
		anthropic.NewBetaTextBlock("Part number: " + name),
	})
}

// ask sends one schema-constrained request and decodes the answer.
func (c *Identifier) ask(ctx context.Context, system string, schema map[string]any, content []anthropic.BetaContentBlockParamUnion) (*Identification, error) {
	resp, err := c.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     c.model,
		MaxTokens: 4096, // the JSON is ~100 tokens; the rest is room for thinking
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(content...)},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffortLow,
			Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
		},
		// If a safety classifier declines, let the server retry on its
		// default fallback model instead of failing.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	})
	if err != nil {
		return nil, err
	}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return nil, fmt.Errorf("claude declined the request (%s)", resp.StopDetails.Category)
	case anthropic.BetaStopReasonMaxTokens:
		return nil, errors.New("claude's answer was cut off (max_tokens)")
	}
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			var id Identification
			if err := json.Unmarshal([]byte(t.Text), &id); err != nil {
				return nil, fmt.Errorf("claude: bad JSON answer: %w", err)
			}
			id.Model = string(resp.Model)
			id.InputTokens, id.OutputTokens = resp.Usage.InputTokens, resp.Usage.OutputTokens
			return &id, nil
		}
	}
	return nil, errors.New("claude: answer has no text")
}

// fit returns w x h scaled down (never up) to fit maxW x maxH.
func fit(w, h int) (int, int) {
	k := math.Min(1, math.Min(float64(maxW)/float64(w), float64(maxH)/float64(h)))
	return max(1, int(float64(w)*k)), max(1, int(float64(h)*k))
}

// shrink scales img down to fit maxW x maxH, averaging the source pixels
// that fall into each output pixel.
func shrink(img image.Image) image.Image {
	b := img.Bounds()
	w, h := fit(b.Dx(), b.Dy())
	if w == b.Dx() && h == b.Dy() {
		return img
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+(y+1)*b.Dy()/h
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+(x+1)*b.Dx()/w
			var r, g, bl, n uint32
			for sy := y0; sy < max(y1, y0+1); sy++ {
				for sx := x0; sx < max(x1, x0+1); sx++ {
					cr, cg, cb, _ := img.At(sx, sy).RGBA()
					r, g, bl, n = r+cr, g+cg, bl+cb, n+1
				}
			}
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), 255
		}
	}
	return out
}
