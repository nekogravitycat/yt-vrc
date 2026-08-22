// Package render draws a message.View to a PNG frame.
package render

import (
	_ "embed"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

// The font is embedded so the runtime has no font dependency (spec
// §4.3.2). Noto Sans TC covers Latin and CJK: display text is English,
// but video titles arrive in whatever script the uploader used.
//
//go:embed NotoSansTC-Regular.otf
var fontData []byte

var (
	parseOnce sync.Once
	parsed    *opentype.Font
	parseErr  error
)

func loadFont() (*opentype.Font, error) {
	parseOnce.Do(func() { parsed, parseErr = opentype.Parse(fontData) })
	return parsed, parseErr
}

// faceCache avoids rebuilding a face per draw; faces are immutable once
// built and safe to reuse from one goroutine at a time.
type faceCache struct {
	mu    sync.Mutex
	faces map[float64]font.Face
}

func newFaceCache() *faceCache { return &faceCache{faces: map[float64]font.Face{}} }

func (c *faceCache) get(size float64) (font.Face, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.faces[size]; ok {
		return f, nil
	}
	f, err := loadFont()
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, err
	}
	c.faces[size] = face
	return face, nil
}
