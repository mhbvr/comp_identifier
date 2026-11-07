# Architecture

comp_identifier reads the marking on an electronic component lying on a white background and, on
request, asks the Claude API what the part is; the web app keeps a database of known components
and the parts in stock. It runs as a web app next to a camera
([camera_rpi](https://github.com/mhbvr/camera_rpi)) or as a command-line tool over photos.

## Packages

```
cmd/compweb ──► camera ──────────────► camera_rpi REST API (MJPEG stream, captures)
     │      ──► tracker ──► marking ──► preprocess   (find, align, crop: pure Go)
     │                         └──────► ocr          (PaddleOCR models on ONNX Runtime)
     │      ──► web ───────► claude ──► Claude API   (on request only)
     │               └─────► inventory ─► SQLite + photo/datasheet files
cmd/compid ───────────────► marking
cmd/photocam                 (fake camera for development)
```

| package | responsibility | external dependencies |
|---|---|---|
| `preprocess` | find the component in a photo, drop shadows and leads, fit a rotated rectangle, cut out an aligned crop | none (standard library) |
| `ocr` | text detection + recognition with PaddleOCR models; models embedded in the binary | ONNX Runtime shared library via [`yalue/onnxruntime_go`](https://github.com/yalue/onnxruntime_go) (cgo) |
| `marking` | crop → OCR at 0/90/180/270° → best reading, confidence, "reliable" flag | — |
| `camera` | read-only client for camera_rpi: MJPEG stream reader, `POST /api/capture`, latest-frame hub | — |
| `tracker` | watches stream frames: component placed / moved / removed; runs recognition on captures | — |
| `web` | HTTP server: `html/template` pages, MJPEG relay, result images, Claude and Add buttons, inventory pages | — |
| `inventory` | component database: components, items, photos, datasheets; marking lookup | SQLite via [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3) (cgo) |
| `claude` | on-demand identification with the Claude API (small, schema-constrained request) | [Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go) |
| `cmd/compweb` | the web app | — |
| `cmd/compid` | CLI: read photos, dump debug images, evaluate against labelled photos | — |
| `cmd/photocam` | serves `/stream.mjpeg` and `/api/capture` from photos | — |

## Recognition pipeline

```
photo ─► Segment ─► rotated rect ─► ExtractRect ─► crop (360 px high, long side horizontal)
                                                      │
                     ┌─────────── 4 rotations ────────┤
                     ▼                                ▼
               ocr.Recognize: detector (DB) ─► line boxes ─► recogniser (CTC) ─► lines + confidences
                     │
                     ▼
        marking: best-scoring rotation ─► text, confidence, upright crop, line images
```

1. **`preprocess.Segment`** finds the body on a downscaled image (≈640 px) and returns its
   minimum-area rectangle in source coordinates. `Process` adds a 10% margin and resamples it into
   the crop.
2. **`ocr.Engine.Recognize`** runs the PP-OCRv3 detector on the crop (scaled so the short side is
   736 px), turns its probability map into line boxes, cuts each line out and runs the PP-OCRv5
   English recogniser on it.
3. **`marking.Reader`** does that for the four rotations of the crop and keeps the rotation whose
   lines score highest (Σ confidence × letters-and-digits). Text at a wrong rotation reads as little
   or nothing.

Details and the reasoning behind each step: [design.md](design.md) and
[text-recognition.md](text-recognition.md).

## Web app data flow

```
camera_rpi /stream.mjpeg ─► camera.Feed ─► camera.Latest ─┬─► web /live.mjpeg ─► browser <img>
                                                          └─► tracker (≈5 Hz) ─► Segment (detection)
camera_rpi /api/capture ◄─ tracker (component still) ─► marking.Reader ─► Reading (text, PNGs)
browser ◄─ web /panel (every 300 ms: SVG rectangle + result panel) ◄─ tracker.Snapshot
browser ─► web POST /claude/{id} ─► claude.Identify ─► Claude API
web /panel ─► inventory.Match (reading text, or Claude's answer) ─► known components
browser ─► web POST /add/{id} ─► inventory.AddItem (crop as photo; new component from Claude's answer)
browser ─► web /inventory/... (forms) ─► inventory
```

- One goroutine keeps the newest stream frame in `camera.Latest`; any number of browsers and the
  tracker read from it.
- The tracker runs one loop: detect on each tick, start a recognition attempt in a goroutine when
  the component is still, merge the result when it returns. Only one recognition runs at a time.
- The page is static HTML plus a fragment (`/panel`) re-fetched by a few lines of JavaScript.

## Runtime requirements

- Go 1.25+ to build (the Anthropic SDK needs it), with cgo (the ONNX Runtime binding and SQLite
  need it). SQLite is compiled into the binary.
- **ONNX Runtime 1.19 or newer**, as a shared library, at run time. The Go binding is pinned to
  `yalue/onnxruntime_go` v1.12.0, which asks for C API version 19; newer binding releases ask for
  the newest API version and refuse older libraries, such as Arch Linux's 1.29. Tested with 1.19.2,
  1.29.0 and 1.31.0, with identical results. Pass `-onnxruntime PATH` or set
  `$ONNXRUNTIME_LIB`; otherwise `libonnxruntime.so` is looked up on the library search path.
  Official builds for Linux x86-64 and aarch64 (Raspberry Pi 4 with a 64-bit OS) are on the
  [ONNX Runtime releases page](https://github.com/microsoft/onnxruntime/releases).
- The OCR models (≈10 MB) are embedded in the binary; see [ocr/models/README.md](../ocr/models/README.md).
- For the Claude button: `ANTHROPIC_API_KEY` or a profile from `ant auth login`.

## Concurrency

- `ocr.Engine` and `marking.Reader` are safe for concurrent use. ONNX Runtime uses its own thread
  pool; `ocr.Config.Threads` caps it per model run. `compid` runs several photos in parallel with one
  thread each; `compweb` runs one recognition at a time and lets ONNX Runtime use all cores.
- ONNX Runtime is loaded once per process; the library path cannot change afterwards.

## Tests

- Unit tests per package, all without network.
- `ocr` and `marking` tests that need ONNX Runtime skip themselves when it can't be loaded.
- `marking/photos_test.go` reads both labelled photo sets and fails below accuracy floors.
- `inventory` tests use a temporary database directory; `web` tests drive the Add buttons and the
  inventory forms against one.
- `camera`, `tracker` and `web` tests use fake HTTP servers, a scripted frame source and a fake
  recogniser; `claude` and `web` tests use a fake Claude API.
