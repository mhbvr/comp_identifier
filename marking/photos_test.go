package marking

import (
	"bufio"
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhbvr/comp_identifier/ocr"
)

// TestRealPhotos runs the whole pipeline over the labelled photo sets and
// guards against accuracy regressions. It needs the ONNX Runtime library
// (see ocr.Config.Library) and is skipped without it.
func TestRealPhotos(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCR over real photos in -short mode")
	}
	r, err := NewReader(Options{OCR: ocr.Config{Threads: 1}})
	if err != nil {
		t.Skip("OCR engine not available:", err)
	}
	defer r.Close()

	// Measured with the PP-OCRv3 detector + PP-OCRv5 English recogniser:
	// 69/77 exact and 71 within one edit; 15/19 and 17. See
	// doc/text-recognition.md. The floors leave a little room.
	for _, set := range []struct {
		dir               string
		minExact, minNear int
	}{
		{"components_photos", 65, 70},
		{"components_photos_2", 13, 16},
	} {
		t.Run(set.dir, func(t *testing.T) {
			exact, near, total := evalSet(t, r, filepath.Join("..", set.dir))
			t.Logf("%s: exact %d/%d, within one edit %d/%d", set.dir, exact, total, near, total)
			if exact < set.minExact || near < set.minNear {
				t.Errorf("accuracy regression: exact %d (want >= %d), near %d (want >= %d)", exact, set.minExact, near, set.minNear)
			}
		})
	}
}

// evalSet reads every photo listed in dir/labels.csv (4 at a time) and
// counts exact and within-one-edit matches of the expected token.
func evalSet(t *testing.T, r *Reader, dir string) (exact, near, total int) {
	fh, err := os.Open(filepath.Join(dir, "labels.csv"))
	if err != nil {
		t.Skip("no labelled photos:", err)
	}
	defer fh.Close()
	type item struct{ file, want string }
	var items []item
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f, w, _ := strings.Cut(line, ",")
		items = append(items, item{f, w})
	}

	dist := make([]int, len(items))
	texts := make([]string, len(items))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				dist[i] = 99
				f, err := os.Open(filepath.Join(dir, items[i].file))
				if err != nil {
					t.Error(err)
					continue
				}
				img, _, err := image.Decode(f)
				f.Close()
				if err != nil {
					t.Error(err)
					continue
				}
				res, err := r.Read(img)
				if err != nil {
					t.Errorf("%s: %v", items[i].file, err)
					continue
				}
				texts[i] = res.Text
				dist[i] = MatchToken(res.Text, items[i].want)
			}
		}()
	}
	for i := range items {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for i, d := range dist {
		if d == 0 {
			exact++
		}
		if d <= 1 {
			near++
		}
		t.Logf("%-26s %-13s d=%-2d %q", items[i].file, items[i].want, d, strings.ReplaceAll(texts[i], "\n", " / "))
	}
	return exact, near, len(items)
}
