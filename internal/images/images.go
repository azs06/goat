// Package images prepares pictures for the model: it checks that a file is an image
// every provider accepts, scales down one larger than they take, and stores the result
// in goat's state, where conversations refer to it by path.
package images

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // registers the GIF decoder
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder
)

const (
	// MaxEdge and MaxPixels bound what is sent. Anthropic rejects images that need
	// scaling, rather than scaling them itself, and charges by area: 1568 pixels on the
	// long side and about 1.15 megapixels is the size it recommends, which costs about
	// 1,600 tokens. OpenAI and DeepSeek scale larger images down to about this size
	// anyway.
	MaxEdge   = 1568
	MaxPixels = 1_150_000
	// maxEncodedBytes keeps the base64 form under 5 MB, the smallest per-image limit of
	// the APIs goat talks to.
	maxEncodedBytes = 3_750_000
	// MaxInputBytes is the largest file goat reads as an image.
	MaxInputBytes = 50 << 20
	// maxDecodePixels refuses images too large to decode safely, such as a crafted
	// header claiming 100000x100000 pixels.
	maxDecodePixels = 100_000_000
	// jpegQuality is used when a scaled image has to be re-encoded as JPEG.
	jpegQuality = 85
)

// EstimatedTokens is roughly what one prepared image costs in a request: about 1,600
// tokens on Claude at the largest size goat sends, at most 1,024 on DeepSeek, and
// about 1,100 on OpenAI.
const EstimatedTokens = 1600

// Prepared is an image ready to send.
type Prepared struct {
	Data      []byte
	MediaType string
	Width     int
	Height    int
	// OriginalWidth and OriginalHeight are the size before scaling; equal to Width
	// and Height when the image was sent as it was.
	OriginalWidth  int
	OriginalHeight int
}

// Scaled reports whether the image was made smaller.
func (p Prepared) Scaled() bool {
	return p.Width != p.OriginalWidth || p.Height != p.OriginalHeight
}

// Describe says what was prepared, such as "1280x720 PNG, 240 KB" or "1568x882 PNG,
// 410 KB, scaled down from 3136x1764".
func (p Prepared) Describe() string {
	text := fmt.Sprintf("%dx%d %s, %s", p.Width, p.Height, FormatName(p.MediaType), formatBytes(len(p.Data)))
	if p.Scaled() {
		text += fmt.Sprintf(", scaled down from %dx%d", p.OriginalWidth, p.OriginalHeight)
	}
	return text
}

// ErrNotImage is returned for data that is not a PNG, JPEG, GIF or WebP image.
var ErrNotImage = errors.New("not a PNG, JPEG, GIF or WebP image")

// MediaType tells the image format from the data itself, as the APIs do, or returns ""
// for anything else.
func MediaType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// FormatName is the short name of a media type, such as "PNG".
func FormatName(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "PNG"
	case "image/jpeg":
		return "JPEG"
	case "image/gif":
		return "GIF"
	case "image/webp":
		return "WebP"
	}
	return mediaType
}

// Extension is the file extension for a media type, with its dot.
func Extension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".png"
}

// Prepare checks data and scales it down when it is larger than the providers take.
// An image within the limits is sent as it is.
func Prepare(data []byte) (Prepared, error) {
	mediaType := MediaType(data)
	if mediaType == "" {
		return Prepared{}, ErrNotImage
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Prepared{}, fmt.Errorf("read the %s image: %w", FormatName(mediaType), err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return Prepared{}, fmt.Errorf("the %s image has no pixels", FormatName(mediaType))
	}
	prepared := Prepared{Data: data, MediaType: mediaType, Width: config.Width, Height: config.Height, OriginalWidth: config.Width, OriginalHeight: config.Height}
	width, height := fit(config.Width, config.Height)
	if width == config.Width && height == config.Height && len(data) <= maxEncodedBytes {
		return prepared, nil
	}
	if int64(config.Width)*int64(config.Height) > maxDecodePixels {
		return Prepared{}, fmt.Errorf("the image is %dx%d, too large to read", config.Width, config.Height)
	}

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Prepared{}, fmt.Errorf("read the %s image: %w", FormatName(mediaType), err)
	}
	scaled := image.NewRGBA(image.Rect(0, 0, width, height))
	if width == config.Width && height == config.Height {
		draw.Draw(scaled, scaled.Bounds(), source, source.Bounds().Min, draw.Src)
	} else {
		xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), source, source.Bounds(), xdraw.Src, nil)
	}
	prepared.Width, prepared.Height = width, height

	// Screenshots and diagrams stay sharp as PNG; photos, and PNGs still too large,
	// become JPEG.
	if mediaType != "image/jpeg" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, scaled); err != nil {
			return Prepared{}, err
		}
		if buf.Len() <= maxEncodedBytes {
			prepared.Data, prepared.MediaType = buf.Bytes(), "image/png"
			return prepared, nil
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Prepared{}, err
	}
	if buf.Len() > maxEncodedBytes {
		return Prepared{}, fmt.Errorf("the image is still %s after scaling, more than the APIs accept", formatBytes(buf.Len()))
	}
	prepared.Data, prepared.MediaType = buf.Bytes(), "image/jpeg"
	return prepared, nil
}

// fit returns the largest size with the image's proportions within MaxEdge and
// MaxPixels, or the size itself when it already fits.
func fit(width, height int) (int, int) {
	scale := 1.0
	if long := max(width, height); long > MaxEdge {
		scale = float64(MaxEdge) / float64(long)
	}
	if area := float64(width) * float64(height) * scale * scale; area > MaxPixels {
		scale *= math.Sqrt(MaxPixels / area)
	}
	if scale >= 1 {
		return width, height
	}
	return max(int(float64(width)*scale), 1), max(int(float64(height)*scale), 1)
}

// Store writes a prepared image into dir under a name taken from its content, readable
// only by the user, and returns its path. The same image is stored once.
func Store(dir string, prepared Prepared) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sum := sha256.Sum256(prepared.Data)
	path := filepath.Join(dir, hex.EncodeToString(sum[:12])+Extension(prepared.MediaType))
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(prepared.Data)) {
		return path, nil
	}
	temp, err := os.CreateTemp(dir, ".image-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(prepared.Data); err != nil {
		temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(temp.Name(), path)
}

func formatBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
