# Text recognition: findings

How the marking reader got to its current form: PaddleOCR detection and recognition models on ONNX
Runtime, reading the aligned colour crop at four rotations. What was tried, what was measured, and
what is still open.

## The task

Markings on IC and transistor packages are short (5–20 characters per line, 1–4 lines), mostly
upper-case letters and digits. They are laser-etched or printed in light grey on black epoxy (dark
on light on some packages) and rendered in narrow, stroke-based fonts. They are photographed under
whatever light is at hand, with glare, grainy epoxy texture and low contrast. Part numbers are not
words, so a language model trained on prose helps little; look-alike characters (`0/O/D`, `5/S`,
`1/I/T`, `8/B`, `2/Z`) are the typical errors.

## Evaluation

- Two labelled photo sets, each with a `labels.csv` holding the part-number token expected in the
  text (spaces ignored):
  - `components_photos/`: 77 photos of 24 parts (DIP, SOIC, TO-92, TO-220), warm light;
  - `components_photos_2/`: 19 photos of 13 parts, cooler, lower-contrast light, where the old
    pipeline fell apart.
- **Exact**: the token appears verbatim. **Near**: within one character edit.
- `compid -eval <set>/labels.csv` prints both; `marking/photos_test.go` fails below fixed floors.

## Results

Same aligned crops for every engine; four rotations, best kept. Times are per photo on one CPU core
of the development machine (x86-64), for all four rotations.

| engine | set 1 (77): exact / near | set 2 (19): exact / near | time |
|---|---|---|---|
| Tesseract 5 LSTM (stock `eng`), binarised + filtered variants (the old pipeline) | 30 / 42 | 5 / 8 | ~0.5–1 s |
| PaddleOCR PP-OCRv3 det + v3 Chinese/English rec, angle classifier (RapidOCR, Python) | 63 / 71 | 17 / 17 | ~0.9 s |
| PP-OCRv3 det + **PP-OCRv5 English rec** (RapidOCR, Python) | 66 / 71 | 15 / 17 | ~0.9 s |
| PP-OCRv5 server det (88 MB) + PP-OCRv5 English rec (RapidOCR, Python) | 68 / 72 | 17 / 18 | ~14 s |
| **PP-OCRv3 det + PP-OCRv5 English rec, this Go implementation** | **69 / 71** | **15 / 17** | ~1.1–1.6 s |

The Go implementation (package `ocr`) is a port of the RapidOCR/PaddleOCR pre- and post-processing,
without the angle classifier (the four-rotation search covers it). The model choice follows the
experiment: the English v5 recogniser has a Latin-only dictionary (no stray Chinese characters) and
was best on set 1. The large v5 detector was a little better again, but ten times slower, which is
too slow for live use.

## Why Tesseract failed

- **Binarisation destroys faint marks.** The old pipeline had to turn the crop into black-on-white
  text for Tesseract: top-hat transform, thresholds, filtering of character-like blobs. Every step
  had a lighting-dependent failure mode:
  - polarity guessed wrong under glare;
  - text blobs merged into words, or into noise and the package edge;
  - thresholds pulled up by bright clutter.

  Each fix (polarity voting, constrained hysteresis, size measured across the text line) helped one
  set of photos. With different lighting, set 2, it fell apart again.
- **The recogniser does not know these fonts.** Even on a clean binary image it misread: the OCR
  input showing `31AE9XM / TL072IP` came out as `2B1AESKM / 2TLO7AIP+`. The stock English model is
  trained on printed documents, not stroke-based laser fonts.
- **Its confidence is a poor signal.** The LSTM engine's word confidence is close to that of the
  worst character, often 0 for nearly right words. The old pipeline had to use per-character
  confidences for scoring.
- **OpenMP made it slow.** Threading made it 4–5× slower on these small images, unless the process
  ran with `OMP_THREAD_LIMIT=1`.

## Why PaddleOCR works

- **No binarisation.** Both networks take the colour image directly; contrast, glare and polarity
  are things they learned to cope with during training.
- **Learned text detection.** The DB detector predicts a per-pixel "text core" probability, so line
  boxes come from the network rather than from hand-tuned thresholds. Our segmentation still finds
  and aligns the component; everything after the crop is learned.
- **Scene-text training data.** PaddleOCR's recognisers are trained on large amounts of photographed
  and synthetic text in many fonts, closer to chip markings than scanned documents. On scene-text
  benchmarks PaddleOCR is generally ahead of Tesseract, for example 91.6% vs 76.8% character
  accuracy on ICDAR 2019 SROIE ([ScienceDirect, 2025][sroie]). I found no published comparison
  specifically on chip markings; studies on laser-engraved metal labels compare several engines
  without a clear winner ([Springer, 2025][springer]). Our own measurements above are the evidence
  that counts here.
- **Small and fast enough.** About 10 MB of models, about 1 s per photo on one CPU core,
  ONNX Runtime builds for x86-64 and aarch64.

## What Claude does differently

- Claude has no separate OCR engine. It is a vision-language model: Anthropic's documentation
  describes images as processed in 28×28-pixel patches, "visual tokens", by the model itself
  ([vision docs][vision]); Anthropic does not publish more detail about the architecture. Its
  reliability on these photos likely comes from two things:
  - **It reads the raw image.** There is no binarisation, so nothing is lost before reading.
  - **It knows the domain.** It knows that `TLC3702CP` and `SN74HC10N` are real part numbers, so an
    ambiguous glyph resolves to the plausible character. Local OCR has no such knowledge.
- A local equivalent exists: small vision-language models such as PaddleOCR-VL (0.9B) or Qwen3-VL
  do OCR end to end. They are reported to be slow on CPU and their local tooling is patchy
  ([1][local1], [2][local2], [3][local3]); on a Raspberry Pi they are impractical. Not tested here.

## Remaining errors (Go implementation)

| kind | example | possible fix |
|---|---|---|
| look-alike characters | `SN74HC1ON`, `C040268E`, `TLCSS5IP`, `74CBTLV32S3D`, `SN74ACT14N` for SN74HC14N | Prefer digits next to digits in part-number-like words, or match against a part database with a confusion-aware distance. |
| two lines merged into one detector box | LM741CN: `LM` / `74` read as one line, `1CN` on its own (visible in the panel's line image) | Tune the detector's unclip ratio and threshold, or use the larger detector for a second attempt when the reading is unreliable. |
| faint or glare-washed marking | two of the four LM317AT (TO-220) photos; MAX941 (`MA / MAX9 / CPA`) | Better light; a fine-tuned recogniser. |
| wrong rotation picked | `8-N088` (`380N-8` upside down) | Rare now; score with a character whitelist. |

## Best practice and next steps

- **Neural detection + recognition on the grey or colour image** is the standard approach for scene
  text and for industrial marking OCR; binarise-then-OCR is not. This project now follows it.
- **Fine-tune the recogniser on real markings.** This is the usual next step in industrial OCR, and
  PaddleOCR supports it. The app already produces labelled data: the aligned crops, the detector's
  line images, and the Claude identification, which is reliable on these photos. A few hundred
  confirmed line crops would let a fine-tuned recogniser learn these fonts.
- **PP-OCRv6** (June 2026) reports better accuracy than v5, and a tiny tier about 4× faster on CPU
  ([arXiv:2606.13108][ppocrv6]). It should be a drop-in replacement once ONNX conversions are
  published.
- **A part-number prior.** A list of known part numbers, or even pattern rules (`SN74HC…`, `LM…`),
  would fix most single-character slips. This is the cheap local version of what Claude does with
  its knowledge.

[sroie]: https://www.sciencedirect.com/science/article/pii/S111001682500657X
[springer]: https://link.springer.com/article/10.1007/s00170-025-15489-9
[vision]: https://platform.claude.com/docs/en/build-with-claude/vision
[local1]: https://pasqualepillitteri.it/en/news/25568/best-local-ocr-pc-server-2026
[local2]: https://prince-arora-aws.medium.com/experiments-benchmarking-local-ocr-models-on-a-scanned-table-in-a-pdf-document-7ab519c717dd
[local3]: https://tinyweights.dev/posts/best-local-vision-language-models-2026/
[ppocrv6]: https://arxiv.org/abs/2606.13108
