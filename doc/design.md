# Design

Decisions behind each part, and what they cost. The text recognition story (why PaddleOCR, what
was measured, what Claude does differently) has its own document:
[text-recognition.md](text-recognition.md).

## Finding and aligning the component (`preprocess`)

The input is a photo of one component on white paper, taken from above, often with uneven light,
a colour cast, soft shadows and shiny leads.

1. **Foreground map.** The photo is downscaled to about 640 px. For every pixel, darkness relative
   to the *local* white level is computed from `min(R,G,B)`: low for black epoxy and for coloured
   packages (a tan package has a low blue channel), high for white paper and for bright metal. The
   local white level is a large max filter followed by a blur, which cancels vignetting and colour
   casts.
2. **Threshold and blob.** Otsu's threshold (at least 35), a small closing, the largest blob, holes
   (the marking) filled.
3. **Empty scene.** A blob smaller than 0.4% of the image is not a component; this is how the
   tracker knows the scene is empty.
4. **Shadows.** A strong shadow passes the threshold and merges with the body, which skews the
   rectangle. The shadow is lighter than the package, so the blob is split once more by Otsu over
   its own pixels, and the dark core is kept if it still covers at least 60% of the blob. Without a
   shadow the core is the whole body; on a glare-washed or light package the core would be too
   small, and the blob is kept instead.
5. **Leads.** An opening sized to the blob (radius ≈ 8% of √area) removes thin protrusions,
   including tarnished, dark leads.
6. **Rectangle.** Minimum-area rectangle of the convex hull (rotating calipers), returned with the
   long side as width.
7. **Crop.** The rectangle plus a 10% margin is resampled with bilinear interpolation into an
   axis-aligned image 360 px high. The margin matters because the rectangle is tight: without it,
   text near the package edge ends up at the crop edge.

Why classical image processing here: the scene is controlled (white background, one part), the
geometry has to be exact for the crop and the on-screen rectangle, and it runs in tens of
milliseconds in pure Go, fast enough for live detection several times a second.

What it doesn't handle: several components at once, non-white backgrounds, a component cut off by
the frame edge (it will be detected, but the crop is partial).

## Reading the marking (`ocr`, `marking`)

- After alignment, text can still be upside down or run along the short side. `marking` reads the
  crop at all four rotations and keeps the one with the highest score
  (Σ over lines of confidence × letters-and-digits). At a wrong rotation the recogniser returns
  little or nothing, so the right one wins clearly.
- Line boxes that are taller than wide are *not* turned sideways before recognition (PaddleOCR
  normally does that for vertical text). With the four-rotation search it would make the 90° reading
  of a sideways crop succeed too, and the "upright" image shown to the user could come out sideways.
- **Confidence** is the mean character probability over the kept lines, weighted by length.
- **Reliable** (the green rectangle) means confidence ≥ 0.93. On the 96 labelled photos, about 95% of
  readings above that threshold have the part number exactly right, and about 80% of all exact
  readings reach it. Raising the threshold does not raise precision: the remaining errors are
  single-character slips (`0`/`O`) read at very high confidence.

## Live tracking (`tracker`)

```
nothing detected ──place──► Working (orange) ──recognised──► Reliable (green) / Unreliable (red)
                               ▲      │
        moved / replaced ──────┘      └─ up to 3 attempts, best kept
removed (3 empty detections) ──► Empty
```

- **Detection** runs on stream frames about every 200 ms: decode the JPEG, `Segment`. It is cheap
  and gives the rectangle drawn over the video.
- **Stability.** Recognition starts after 3 detections in the same place. "Same place": the centre
  moved less than 3% of the frame diagonal, the area changed less than 15%, the angle less than 8°
  (ignored for nearly square bodies, whose long axis is unstable). A hand reaching in, a nudge or a
  swap counts as a new component.
- **Removal.** 3 detections without a component.
- **Attempts.** Each attempt takes a new full-resolution capture (`POST /api/capture`, 2028×1520, the
  same as the sample photos; the stream frame is used if the capture fails). Up to 3 attempts; a
  reliable reading wins over an unreliable one, otherwise the higher score. It stops early on a
  reliable reading.
- **One recognition at a time**: results for a component that has since left or changed are
  discarded.
- **No history**: when the component leaves, its reading is dropped.

## Camera (`camera`)

- Read-only use of the camera_rpi API: `GET /stream.mjpeg` (frames for the live view and detection)
  and `POST /api/capture` (stills for recognition). Camera settings are never changed.
- The stream is read once and fanned out to every browser by `camera.Latest`, which keeps only the
  newest frame. Slow readers skip frames instead of building up delay.
- The stream reconnects with a growing delay (1 s to 10 s). `-camera-ca` trusts the CA that
  camera_rpi's `make cert` creates, for an `https://` camera.

## Web UI (`web`)

- **Server-rendered.** Two `html/template` files. The live video is `<img src="/live.mjpeg">`, so it
  needs no JavaScript.
- **Dynamic part.** The SVG rectangle over the video and the result panel are one template fragment,
  `/panel`. About ten lines of inline JavaScript fetch it every 300 ms and swap the two elements by
  id. No third-party front-end code.
- **Rectangle.** An SVG polygon with `viewBox` set to the frame size and
  `preserveAspectRatio="none"`, so it lines up with the scaled video at any window size.
- **Panel, top to bottom:** the crop turned so the text reads upright; the text lines the recogniser
  read (exactly what it saw, which explains most errors at a glance); the text; confidence,
  attempts, time and rotation; the Claude section.
- **Images** are served as PNG at `/img/{id}/{crop|ocr}.png?a={attempts}`. A given URL never changes,
  so the browser caches it and the panel doesn't flicker between refreshes.

## Identification with Claude (`claude`)

- **On request only**: the button or the `i` key, for the reading on screen, at most once per reading
  (a failed request can be retried).
- **Small requests.** About 600 input tokens:
  - the upright colour crop scaled to fit 512×384 as JPEG (images cost ⌈w/28⌉×⌈h/28⌉ visual tokens,
    so at most 266);
  - a four-sentence system prompt;
  - the local OCR text as a one-line hint.
- **Small answers.** Schema-constrained JSON (marking, part number, manufacturer, description,
  datasheet URL, confidence) at `effort: low`.
- **No web search.** It would add thousands of input tokens per part. The datasheet URL comes from
  Claude's knowledge and is labelled unverified; a free search link built from the part number is
  always shown.
- **No prompt caching.** The fixed part of the request is below the minimum cacheable size.
- **Refusal fallback.** Server-side refusal fallback is on (`fallbacks: "default"`); the panel shows
  which model answered and the tokens used.
- **Model.** The default is `claude-opus-5-5`; `-claude-model` changes it.

## Removed along the way

- **Tesseract** (libtesseract via cgo, or the CLI) and everything built around it:
  - binarisation, top-hat and character-blob filtering;
  - polarity detection and image variants;
  - re-executing the process with `OMP_THREAD_LIMIT=1` (OpenMP made it 4–5× slower on small images).

  Replaced by the PaddleOCR models; see [text-recognition.md](text-recognition.md).
- **Component history** in the web app: not needed at this stage.
- **Agreement between captures** as a "reliable" signal: on a still component it shows that the
  reading is stable, not that it is right.
