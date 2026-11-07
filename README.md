# comp_identifier

Identify electronic components (ICs, MOSFETs, BJTs, regulators, …) from photos.

It reads the **marking text** on a component lying on a white background, either live under a
camera in a web app (`compweb`) or from photos (`compid`). Recognition runs locally, with PaddleOCR
models on ONNX Runtime. There is no component database yet: the result is the text on the part.
On request, the web app also asks the Claude API to identify the part: canonical part number,
manufacturer, description and datasheet link.

```
photo ─► find + align component ─► crop ─► text detection + recognition × 4 rotations ─► best text
```

| doc | contents |
|---|---|
| [doc/architecture.md](doc/architecture.md) | packages, data flow, runtime requirements, concurrency, tests |
| [doc/design.md](doc/design.md) | how each part works and why: segmentation, rotation search, tracking, web UI, Claude |
| [doc/text-recognition.md](doc/text-recognition.md) | OCR findings: Tesseract vs PaddleOCR measurements, what Claude does differently, remaining errors, next steps |

## Install

Requirements: Go 1.25+ with cgo, and the **ONNX Runtime 1.19 or newer** shared library. The OCR
models are embedded in the binary.

ONNX Runtime from your distribution, if it packages it (it is then found as `libonnxruntime.so`
without further setup). On Arch Linux:

```bash
sudo pacman -S onnxruntime-cpu
```

Otherwise use an official build. For Linux x86-64 (for a Raspberry Pi 4 with a 64-bit OS, use
`onnxruntime-linux-aarch64-1.29.0.tgz`):

```bash
curl -LO https://github.com/microsoft/onnxruntime/releases/download/v1.29.0/onnxruntime-linux-x64-1.29.0.tgz
```

```bash
tar xzf onnxruntime-linux-x64-1.29.0.tgz
```

```bash
export ONNXRUNTIME_LIB=$PWD/onnxruntime-linux-x64-1.29.0/lib/libonnxruntime.so.1.29.0
```

Instead of the environment variable you can pass `-onnxruntime PATH` to either program.

Build:

```bash
go build ./cmd/compweb ./cmd/compid
```

## Web app

```bash
./compweb -camera http://raspberrypi.local:8080 -listen :8090
```

Open `http://localhost:8090`. The live camera view shows a rectangle around the component:

- **orange**: detected, being recognised;
- **green**: text read with high confidence;
- **red**: text read with low confidence.

Next to the video, top to bottom:
- the component, turned so its text reads upright;
- the text lines the recogniser read;
- the recognised text;
- confidence, attempts, time and rotation.

Take the component away and put the next one down. Camera settings are never changed.

**Identify with Claude:** press the button or the **i** key. It costs about 600 input tokens per
part; nothing is sent without your request. Credentials come from `ANTHROPIC_API_KEY` or `ant auth
login`. `-claude=false` hides the button and `-claude-model` picks the model (default
`claude-opus-5-5`). Details: [doc/design.md](doc/design.md#identification-with-claude-claude).

Without a Pi, the fake camera serves photos, alternating with empty paper:

```bash
go run ./cmd/photocam -listen :8080 components_photos_2/*.jpg
```

```bash
go run ./cmd/compweb -camera http://localhost:8080
```

## Command line

```bash
./compid components_photos_2/IMG_20261107_171749.jpg
```

```
components_photos_2/IMG_20261107_171749.jpg: "31AE9XM / TL072IP" (confidence 98%, read at 0°, 1.2s)
```

Flags:
- `-all`: list the reading at every rotation.
- `-json`: one JSON object per photo.
- `-debug DIR`: write the detected rectangle, crop, upright crop and line images.
- `-eval labels.csv`: measure accuracy.

As a library:

```go
r, err := marking.NewReader(marking.Options{}) // ocr.Config{Library: ...} if not in $ONNXRUNTIME_LIB
if err != nil { ... }
defer r.Close()
res, err := r.Read(img)   // img is any image.Image; preprocess.ErrNoComponent if empty
fmt.Println(res.Text)     // lines separated by "\n"
fmt.Println(res.Reliable) // confidence ≥ 0.93
```

## Accuracy

Two labelled photo sets (`components_photos*/labels.csv`, expected part number per photo):

```bash
./compid -eval components_photos/labels.csv
```

| set | part number exact | within one edit |
|---|---|---|
| `components_photos` (77 photos, 24 parts) | 69 (90%) | 71 (92%) |
| `components_photos_2` (19 photos, 13 parts, harder light) | 15 (79%) | 17 (89%) |

The previous Tesseract pipeline managed 30 and 5. Reading takes about 1–1.5 s per photo on one CPU core.
`go test ./...` runs both sets as a regression test (skipped without ONNX Runtime or with `-short`).
What's left and what to do about it: [doc/text-recognition.md](doc/text-recognition.md#remaining-errors-go-implementation).
