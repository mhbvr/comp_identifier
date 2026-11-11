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
        (2 detections elsewhere)
removed (3 empty detections) ──► Empty
```

- **Detection** runs on stream frames about every 200 ms: decode the JPEG, `Segment`. It is cheap
  and gives the rectangle drawn over the video.
- **Stability.** Recognition starts after 3 detections at the same place.
- **Same place** means that at least half of one rectangle lies inside the other, compared with
  where the component was first detected.
  - **Why containment.** Lighting changes make the detected outline gain or lose the shadow and the
    leads from frame to frame. That changes its size by up to ~75%, shifts its centre by up to
    ~7% of the frame diagonal, and for nearly square outlines swaps width and height, which looks
    like a 90° turn. Containment ignores all of that.
  - **History.** The first version compared centre, size and angle against fixed limits, and
    restarted recognition on still parts under changing light. Exposure, contrast and noise
    variations alone made 19 of 39 photos restart; with containment, none did (lowest overlap
    0.96).
- **Moved** means 2 consecutive detections at another place. A single odd detection, such as glare
  or a hand passing, is ignored. A component moved by more than about half its size, or replaced,
  starts over; a small nudge keeps the reading.
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
  and text (a failed request can be retried).
- **Text for Claude.** A box on the camera page takes text the user read on the part, for markings
  the camera can't see (the side of a cylindrical package, the underside). It goes as a third text
  block after the OCR hint, and the system prompt tells Claude to trust it over the photo and to
  include it in the marking, so it ends up in the item's marking when the part is added. The box
  sits outside the panel that is re-rendered every 300 ms, so typing isn't lost, and the `i`/`a`
  shortcuts are ignored while typing. Asking again with a different text sends a new request.
- **Describing a part number** (`Describe`), for components added by hand: a text-only request (part
  number in, the same answer without the marking out), a few hundred input tokens. The form's
  **Ask Claude** button calls `POST /inventory/describe` and fills in the description and datasheet
  URL; the URL is downloaded in the background once the component is added, if it serves a PDF.
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

## Component database (`inventory`)

- **Layout.** A directory with `inventory.db` (SQLite), `photos/` and `datasheets/`; the tables store
  file names, not paths, so the directory can be moved or copied as a whole.
  - `components(id, name UNIQUE COLLATE NOCASE, description, datasheet)`: a part model. The name is
    the part number and identifies the component regardless of case.
  - `items(id, component_id → components, marking, photo)`: one physical part. The marking is stored
    one line per text line; the photo is the upright crop as JPEG (`item<ID>.jpg`).
- **Lookup** of a reading (`Match`), forgiving one OCR edit per 6 letters (none under 6 letters,
  where one edit turns one part number into another), against:
  - component names, as a substring of the reading (part numbers are printed on the part);
  - item markings, whole (the same batch reads the same);
  - item marking lines of 6+ letters, as a substring (the printed part number when the component's
    name is a canonical one that isn't printed, such as LM35DZ/NOPB).

  The best match has the fewest edits, then the longest matched text (TLC555IP before TLC555). The
  other matches are offered as alternatives. A lookup scans all names and markings, which is
  instant for a hobby inventory; the panel caches it per reading and database change.
- **With Claude's answer**, the lookup uses it instead of the local reading: exact part number
  first, then Claude's reading of the marking. If no match has Claude's part number, the panel
  offers to add it as a new component (description `description (manufacturer)`).
- **Adding** happens only when recognition has finished, once per reading. The item's marking is
  Claude's reading when Claude was asked (it reads better), else the local one. A new component and
  its first item are created in one transaction. If Claude gave a datasheet URL, it is downloaded
  in the background and kept only if it is a PDF; the URL comes from Claude's knowledge and is often
  wrong, so failures are only logged and the datasheet can be uploaded on the component's page.
- **No new component without Claude** on the camera page: an unknown part is either read again
  (move it) or identified by Claude, so names stay canonical part numbers rather than OCR guesses.
  The inventory pages can still add and edit anything by hand.
- **Inventory pages** are plain server-rendered HTML forms (POST, then redirect), with no
  JavaScript besides delete confirmations and the **Ask Claude** button of the new-component form.

## Removed along the way

- **Tesseract** (libtesseract via cgo, or the CLI) and everything built around it:
  - binarisation, top-hat and character-blob filtering;
  - polarity detection and image variants;
  - re-executing the process with `OMP_THREAD_LIMIT=1` (OpenMP made it 4–5× slower on small images).

  Replaced by the PaddleOCR models; see [text-recognition.md](text-recognition.md).
- **Component history** in the web app: not needed at this stage.
- **Agreement between captures** as a "reliable" signal: on a still component it shows that the
  reading is stable, not that it is right.
