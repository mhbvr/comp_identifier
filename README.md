# comp_identifier

Identify electronic components (ICs, MOSFETs, BJTs, regulators, …) from photos.

This first stage reads the **marking text** on a component photographed on a
white background. It's written in Go and runs fully locally (Tesseract OCR).

```
photo ──► segment ──► rotate + crop ──► enhance ──► OCR × 4 orientations ──► best text
```

| package | what it does | deps |
|---|---|---|
| `preprocess` | finds the component, aligns and crops it, builds OCR-ready images | pure Go, stdlib only |
| `ocr` | small engine interface over Tesseract: in-process `LibEngine` (C API via cgo) or `CLIEngine` (runs `tesseract`) | libtesseract / tesseract binary |
| `marking` | glue: preprocess → OCR every variant at 0/90/180/270° → pick the most confident reading | — |
| `cmd/compid` | command-line tool, debug image dump, accuracy evaluation | — |

## Install

```bash
sudo apt-get install -y tesseract-ocr tesseract-ocr-eng libtesseract-dev libleptonica-dev pkg-config
```

```bash
go build ./cmd/compid
```

You can also build with `CGO_ENABLED=0`. Then only the `tesseract` binary is needed at run time, and the
`CLIEngine` is used automatically (it is about 3× slower).

## Usage

```bash
./compid components_photos/IMG_20261107_115535.jpg
```

```
components_photos/IMG_20261107_115535.jpg: "JCBC / 33740 / R25" (conf 99, tophat@180°, 478ms)
```

Useful flags: `-all` lists every OCR candidate. `-json` prints one JSON object per photo. `-debug DIR` writes
the intermediate images. `-eval labels.csv` measures accuracy (see below). `-tessdata DIR` uses other models.

As a library:

```go
r, err := marking.NewReader(marking.DefaultOptions())
if err != nil { ... }
defer r.Close()            // a Reader is not goroutine-safe; use one per goroutine
res, err := r.Read(img)    // img is any image.Image
fmt.Println(res.Text)      // lines separated by "\n"
// res.Candidates: all readings, best first. res.Preprocess: crop, masks, variants.
```

`preprocess.Process(img, preprocess.Options{})` can be used on its own. It needs nothing outside the standard library.

### Performance note: `OMP_THREAD_LIMIT=1`

Tesseract's OpenMP threading makes OCR of these small images about **4–5× slower**: 4.4 s vs 0.9 s per photo
on one core. libgomp reads the limit only at process start. `compid` and the test binary re-exec themselves
with `OMP_THREAD_LIMIT=1`. **Programs that use the library should set it in their environment too.** To use
several cores, run several `Reader`s in parallel, as `compid` does.

## How it works

1. **Segment** (`preprocess/segment.go`). The photo is downscaled to about 640 px. A *foreground map* measures
   how much darker each pixel is than the local white level. It is based on `min(R,G,B)`, so black epoxy and
   coloured packages stand out but shiny leads do not. The local white level is estimated with a large max
   filter plus a blur, which cancels vignetting and colour casts. The steps after that:
   - Otsu threshold, closing, then the largest blob with its holes filled.
   - An opening sized to the blob removes leads, including tarnished dark ones.
   - The minimum-area rotated rectangle of the convex hull gives the body (rotating calipers).
2. **Rotate + crop** (`warp.go`). The rectangle plus a small margin is resampled (bilinear) into an
   axis-aligned image, long side horizontal, 360 px tall.
3. **Enhance** (`enhance.go`, `textmask.go`):
   - *Body mask* inside the crop. An opening removes leads, and the rim is eroded away so package edges
     don't count.
   - *Top-hat transform*: the image minus its morphological opening. It keeps thin bright strokes and removes
     glare gradients, the body and the paper.
   - *Character extraction*: a threshold capped by a noise estimate (median + 6·MAD) avoids being pulled up
     by bright clutter. Strong strokes grow into adjacent weak pixels. Connected components that touch the
     body edge, are tiny, or have the wrong size are dropped. Size is measured *across* the text line, because
     touching letters merge into word-sized blobs. Whichever box dimension clusters best gives the text
     direction and the character height.
   - *Polarity*: light-on-dark (laser marks) or dark-on-light (e.g. a tan package). Both are tried, and the
     one that yields more consistent character blobs wins. When it's close, both are sent to OCR.
   - *Variants*, rescaled so characters are about 44 px tall (best for the stock model) and padded:
     `clean` (kept components, binary), `soft` (top-hat around them), `tophat` (whole body).
4. **OCR** (`ocr`, `marking`). Each variant is OCR'd at 0/90/180/270° with Tesseract LSTM. The settings are
   PSM 6, a whitelist `A-Z 0-9 - / . +`, and no dictionaries. Candidates are scored by
   Σ chars × (mean char confidence)³, with 1–2 character words discounted. Per-character confidence matters
   because Tesseract's LSTM *word* confidence is about the worst character's, often 0 for almost-right words.

## Accuracy on the sample photos

`components_photos/labels.csv` lists the expected part-number token for each of the 77 photos (24 distinct
components: DIP, SOIC, TO-92, TO-220).

```bash
./compid -eval components_photos/labels.csv
```

| | photos |
|---|---|
| part number read exactly | 30 / 77 (39 %) |
| within one character edit | 44 / 77 (57 %) |
| time | ~0.9 s per photo per core |

`go test ./...` runs this as a regression test (`marking/photos_test.go`). Use `-short` to skip it.

Typical remaining errors are look-alike characters in laser-etched fonts: `0/O/D`, `5/S`, `1/I/T`, `8/B`, a
dropped `1`. There are also faint or glare-washed markings, especially TO-220 regulators.

## Limitations and next steps

- **Better models.** The Ubuntu `tesseract-ocr-eng` package ships the small "fast" model. Try `tessdata_best`
  or a model fine-tuned on component markings with `-tessdata DIR` / `ocr.Config.TessdataPrefix`.
- **Fuzzy matching against a part database** is the natural next stage. With a confusion-aware distance
  (`0≈O≈D`, `5≈S`, `1≈I`, `8≈B`, `2≈Z`), most "within one edit" readings resolve to the right part.
  `marking.MatchToken` and `Result.Candidates`, which keep several readings per photo, are hooks for it.
- One component per photo is assumed. A plain white background works best.
