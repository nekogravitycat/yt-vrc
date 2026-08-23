package render

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"github.com/nekogravitycat/yt-vrc/internal/domain/message"
)

// Frame geometry. 1280x720 balances legibility against file size; on a
// VR video panel this resolves text cleanly (spec §4.3.3).
const (
	Width  = 1280
	Height = 720

	headerH = 132
	padX    = 56

	titleSize  = 54
	bodySize   = 38 // >= Height/20, the legibility floor of spec §4.3.3
	footerSize = 28
	lineH      = 56
	// valueX: the label column has to hold a cache row's
	// "133.5 MB · 1080p HLS" without truncating, which is the widest
	// label any view produces; the value column keeps ample room for a
	// video title even so.
	valueX = 520
)

// A dark ground with bright text: VR displays render high-contrast
// combinations far more reliably than subtle ones (spec §4.3.3).
var (
	colBG     = color.RGBA{0x16, 0x19, 0x1D, 0xFF}
	colText   = color.RGBA{0xF2, 0xF4, 0xF6, 0xFF}
	colMuted  = color.RGBA{0x9A, 0xA4, 0xAE, 0xFF}
	colBarBG  = color.RGBA{0x2A, 0x2F, 0x36, 0xFF}
	colHeadFG = color.RGBA{0x11, 0x14, 0x17, 0xFF}
)

// kindColor maps each category onto its header colour (spec §4.3.4).
func kindColor(k message.Kind) color.RGBA {
	switch k {
	case message.KindStatus:
		return color.RGBA{0x4A, 0x9E, 0xFF, 0xFF} // blue
	case message.KindProgress:
		return color.RGBA{0xF5, 0xC2, 0x42, 0xFF} // yellow
	case message.KindSuccess:
		return color.RGBA{0x4C, 0xC3, 0x8A, 0xFF} // green
	case message.KindWarning:
		return color.RGBA{0xF0, 0x8C, 0x3A, 0xFF} // orange
	case message.KindError:
		return color.RGBA{0xE5, 0x5B, 0x5B, 0xFF} // red
	default:
		return color.RGBA{0x7A, 0x84, 0x8E, 0xFF} // grey: gate closed
	}
}

// Renderer draws views to PNG frames.
type Renderer struct {
	mu    sync.Mutex
	faces *faceCache
}

func New() *Renderer { return &Renderer{faces: newFaceCache()} }

// RenderPNG draws v and writes a PNG to w.
func (r *Renderer) RenderPNG(v message.View, w io.Writer) error {
	// NOTE: font.Face draws are not concurrency-safe; serialise (cheap --
	// single-digit ms per frame). Canonical for faceCache too (font.go).
	r.mu.Lock()
	defer r.mu.Unlock()

	titleFace, err := r.faces.get(titleSize)
	if err != nil {
		return err
	}
	bodyFace, err := r.faces.get(bodySize)
	if err != nil {
		return err
	}
	footFace, err := r.faces.get(footerSize)
	if err != nil {
		return err
	}

	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	fill(img, img.Bounds(), colBG)

	head := kindColor(v.Kind)
	fill(img, image.Rect(0, 0, Width, headerH), head)
	drawText(img, titleFace, colHeadFG, padX, 88, truncate(titleFace, v.Title, Width-2*padX))

	y := headerH + 64
	// Full contrast + extra gap: distinguishes the subtitle (usually the
	// video title) from an ordinary row label.
	if v.Subtitle != "" {
		drawText(img, bodyFace, colText, padX, y, truncate(bodyFace, v.Subtitle, Width-2*padX))
		y += lineH + 18
	}

	for _, row := range v.Rows {
		if y > Height-120 {
			break
		}
		drawText(img, bodyFace, colMuted, padX, y, truncate(bodyFace, row.Label, valueX-padX-16))
		drawText(img, bodyFace, colText, valueX, y, truncate(bodyFace, row.Value, Width-valueX-padX))
		y += lineH
	}

	for _, line := range v.Lines {
		if y > Height-120 {
			break
		}
		drawText(img, bodyFace, colText, padX, y, truncate(bodyFace, line, Width-2*padX))
		y += lineH
	}

	if v.Progress != nil {
		p := *v.Progress
		p = max(0, min(1, p))
		barY := y + 10
		if barY > Height-150 {
			barY = Height - 150
		}
		barW := Width - 2*padX
		fill(img, image.Rect(padX, barY, padX+barW, barY+34), colBarBG)
		fill(img, image.Rect(padX, barY, padX+int(float64(barW)*p), barY+34), head)
	}

	if v.Footer != "" {
		drawText(img, footFace, colMuted, padX, Height-44, truncate(footFace, v.Footer, Width-2*padX))
	}

	return png.Encode(w, img)
}

func fill(dst *image.RGBA, r image.Rectangle, c color.RGBA) {
	draw.Draw(dst, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

func drawText(dst *image.RGBA, face font.Face, c color.RGBA, x, baseline int, s string) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	d.DrawString(s)
}

// truncate shortens s with an ellipsis until it fits maxW pixels.
func truncate(face font.Face, s string, maxW int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if textWidth(face, s) <= maxW {
		return s
	}
	runes := []rune(s)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		if textWidth(face, string(runes)+"…") <= maxW {
			return string(runes) + "…"
		}
	}
	return ""
}

func textWidth(face font.Face, s string) int {
	return font.MeasureString(face, s).Ceil()
}
