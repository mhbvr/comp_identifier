package marking

import (
	"bufio"
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mhbvr/comp_identifier/ocr"
)

// TestMain re-executes the test binary with OMP_THREAD_LIMIT=1: Tesseract's
// OpenMP threading makes OCR of small images several times slower, and the
// limit is only read at process start.
func TestMain(m *testing.M) {
	ocr.LimitOpenMPThreads()
	os.Exit(m.Run())
}

// TestRealPhotos runs the whole pipeline over the labelled photos in
// components_photos and guards against accuracy regressions.
func TestRealPhotos(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping OCR over real photos in -short mode")
	}
	dir := filepath.Join("..", "components_photos")
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

	probe, err := NewReader(DefaultOptions())
	if err != nil {
		t.Skip("Tesseract not available:", err)
	}
	probe.Close()

	dist := make([]int, len(items))
	texts := make([]string, len(items))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(4, runtime.NumCPU()); w++ {
		r, err := NewReader(DefaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
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

	exact, near := 0, 0
	for i, d := range dist {
		if d == 0 {
			exact++
		}
		if d <= 1 {
			near++
		}
		t.Logf("%-24s %-13s d=%-2d %q", items[i].file, items[i].want, d, strings.ReplaceAll(texts[i], "\n", " / "))
	}
	t.Logf("exact %d/%d, within one edit %d/%d", exact, len(items), near, len(items))
	// Measured: 30 exact, 44 within one edit (stock Ubuntu eng model).
	if exact < 26 || near < 40 {
		t.Errorf("accuracy regression: exact %d (want >= 26), near %d (want >= 40)", exact, near)
	}
}
