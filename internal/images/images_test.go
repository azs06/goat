package images

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gradient(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	return img
}

// tinyWebP is a 1x1 lossless WebP image.
const tinyWebP = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

func TestMediaTypeReadsTheData(t *testing.T) {
	webp, _ := base64.StdEncoding.DecodeString(tinyWebP)
	var jpg, gifData bytes.Buffer
	jpeg.Encode(&jpg, gradient(4, 4), nil)
	gif.Encode(&gifData, gradient(4, 4), nil)
	for data, want := range map[string]string{
		string(encodePNG(t, gradient(2, 2))): "image/png",
		jpg.String():                         "image/jpeg",
		gifData.String():                     "image/gif",
		string(webp):                         "image/webp",
		"<svg></svg>":                        "",
		"":                                   "",
	} {
		if got := MediaType([]byte(data)); got != want {
			t.Errorf("MediaType(%q...) = %q, want %q", data[:min(len(data), 8)], got, want)
		}
	}
}

func TestPrepareKeepsAnImageThatFits(t *testing.T) {
	data := encodePNG(t, gradient(640, 480))
	prepared, err := Prepare(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prepared.Data, data) || prepared.Scaled() || prepared.MediaType != "image/png" {
		t.Fatalf("expected the image unchanged, got %s", prepared.Describe())
	}
	if !strings.HasPrefix(prepared.Describe(), "640x480 PNG, ") {
		t.Errorf("got %q", prepared.Describe())
	}

	webp, _ := base64.StdEncoding.DecodeString(tinyWebP)
	if prepared, err := Prepare(webp); err != nil || prepared.MediaType != "image/webp" || prepared.Width != 1 {
		t.Errorf("WebP: %+v %v", prepared, err)
	}
}

func TestPrepareScalesALargeImageDown(t *testing.T) {
	prepared, err := Prepare(encodePNG(t, gradient(3200, 1800)))
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Scaled() || prepared.Width > MaxEdge || prepared.Width*prepared.Height > MaxPixels || prepared.OriginalWidth != 3200 {
		t.Fatalf("got %s", prepared.Describe())
	}
	// The proportions are kept.
	if ratio := float64(prepared.Width) / float64(prepared.Height); ratio < 1.75 || ratio > 1.80 {
		t.Errorf("aspect ratio %.3f", ratio)
	}
	if prepared.MediaType != "image/png" || MediaType(prepared.Data) != "image/png" {
		t.Errorf("a scaled screenshot stays PNG, got %s", prepared.MediaType)
	}
	if !strings.Contains(prepared.Describe(), "scaled down from 3200x1800") {
		t.Errorf("got %q", prepared.Describe())
	}
}

func TestPrepareTurnsAHugeNoisyPNGIntoJPEG(t *testing.T) {
	// Random pixels, transparency included, do not compress: even scaled down, the PNG
	// is over the limit.
	noise := image.NewNRGBA(image.Rect(0, 0, 2000, 2000))
	rand.New(rand.NewSource(1)).Read(noise.Pix)
	prepared, err := Prepare(encodePNG(t, noise))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.MediaType != "image/jpeg" || len(prepared.Data) > maxEncodedBytes {
		t.Fatalf("expected a JPEG under the limit, got %s", prepared.Describe())
	}
}

func TestPrepareRefusesWhatIsNotAnImage(t *testing.T) {
	if _, err := Prepare([]byte("hello")); !errors.Is(err, ErrNotImage) {
		t.Errorf("got %v", err)
	}
	// A PNG signature with nothing usable after it.
	if _, err := Prepare([]byte("\x89PNG\r\n\x1a\nbroken")); err == nil {
		t.Error("expected a broken PNG to be refused")
	}
}

func TestFitKeepsWithinBothLimits(t *testing.T) {
	for _, size := range [][2]int{{1568, 100}, {100, 5000}, {1500, 1500}, {10, 10}} {
		w, h := fit(size[0], size[1])
		if max(w, h) > MaxEdge || w*h > MaxPixels || w < 1 || h < 1 {
			t.Errorf("fit(%v) = %dx%d", size, w, h)
		}
	}
	if w, h := fit(800, 600); w != 800 || h != 600 {
		t.Errorf("a small image changed size: %dx%d", w, h)
	}
}

func TestStoreKeepsOnePrivateCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	prepared, _ := Prepare(encodePNG(t, gradient(8, 8)))
	first, err := Store(dir, prepared)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Store(dir, prepared)
	if err != nil || second != first || filepath.Ext(first) != ".png" {
		t.Fatalf("got %s and %s (%v)", first, second, err)
	}
	data, _ := os.ReadFile(first)
	if !bytes.Equal(data, prepared.Data) {
		t.Error("the stored file differs")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(first); info.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", info.Mode().Perm())
		}
		if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestNamesAndExtensions(t *testing.T) {
	for mediaType, want := range map[string][2]string{"image/png": {"PNG", ".png"}, "image/jpeg": {"JPEG", ".jpg"}, "image/gif": {"GIF", ".gif"}, "image/webp": {"WebP", ".webp"}} {
		if FormatName(mediaType) != want[0] || Extension(mediaType) != want[1] {
			t.Errorf("%s: %s %s", mediaType, FormatName(mediaType), Extension(mediaType))
		}
	}
	if formatBytes(512) != "512 bytes" || formatBytes(2048) != "2 KB" || formatBytes(3<<20) != "3.0 MB" {
		t.Error("formatBytes")
	}
}

// testGIF encodes frames as a GIF of the given canvas size; each frame is a solid
// square of its color at its bounds.
func testGIF(t *testing.T, width, height int, frames map[image.Rectangle]color.Color, order []image.Rectangle) []byte {
	t.Helper()
	palette := color.Palette{color.Transparent, color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	anim := &gif.GIF{Config: image.Config{Width: width, Height: height, ColorModel: palette}}
	for _, bounds := range order {
		frame := image.NewPaletted(bounds, palette)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				frame.Set(x, y, frames[bounds])
			}
		}
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAnimatedGIFGoesAsItsFirstFrame(t *testing.T) {
	red, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}
	first, second := image.Rect(10, 10, 20, 20), image.Rect(0, 0, 40, 30)
	data := testGIF(t, 40, 30, map[image.Rectangle]color.Color{first: red, second: blue}, []image.Rectangle{first, second})
	if !animatedGIF(data) {
		t.Fatal("expected the GIF to count as animated")
	}

	prepared, err := Prepare(data)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.MediaType != "image/png" || prepared.Width != 40 || prepared.Height != 30 || prepared.Scaled() {
		t.Fatalf("got %s %dx%d (from %dx%d)", prepared.MediaType, prepared.Width, prepared.Height, prepared.OriginalWidth, prepared.OriginalHeight)
	}
	still, err := png.Decode(bytes.NewReader(prepared.Data))
	if err != nil {
		t.Fatal(err)
	}
	// The first frame sits where it was drawn on the canvas; the rest is transparent,
	// and nothing of the second frame shows.
	if r, g, b, a := still.At(15, 15).RGBA(); r>>8 != 255 || g != 0 || b != 0 || a>>8 != 255 {
		t.Errorf("inside the first frame: %v", still.At(15, 15))
	}
	if _, _, _, a := still.At(2, 2).RGBA(); a != 0 {
		t.Errorf("outside the first frame should be transparent: %v", still.At(2, 2))
	}
}

func TestStillGIFIsSentAsItIs(t *testing.T) {
	only := image.Rect(0, 0, 12, 8)
	data := testGIF(t, 12, 8, map[image.Rectangle]color.Color{only: color.RGBA{255, 0, 0, 255}}, []image.Rectangle{only})
	if animatedGIF(data) {
		t.Fatal("a one-frame GIF is not animated")
	}
	prepared, err := Prepare(data)
	if err != nil || prepared.MediaType != "image/gif" || !bytes.Equal(prepared.Data, data) {
		t.Fatalf("got %s (%v)", prepared.MediaType, err)
	}
}

func TestAnimatedGIFCheckSurvivesBrokenData(t *testing.T) {
	first, second := image.Rect(0, 0, 4, 4), image.Rect(0, 0, 4, 4)
	data := testGIF(t, 4, 4, map[image.Rectangle]color.Color{first: color.RGBA{255, 0, 0, 255}}, []image.Rectangle{first, second})
	for cut := 0; cut < len(data); cut++ {
		animatedGIF(data[:cut]) // must not panic
	}
	for _, junk := range [][]byte{nil, []byte("GIF89a"), append([]byte("GIF89a\x04\x00\x04\x00\xff\x00\x00"), 0x2c)} {
		if animatedGIF(junk) {
			t.Errorf("%q counted as animated", junk)
		}
	}
}
