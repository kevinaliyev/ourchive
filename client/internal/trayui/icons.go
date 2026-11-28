package trayui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"

	"github.com/ourchive/ourchive/internal/agentstatus"
)

// Tray icons are drawn at startup rather than shipped as files: a ring whose colour is the
// contributor status (grey idle, green ok, amber reconnecting, red offline).
var (
	iconMu    sync.Mutex
	iconCache = map[agentstatus.Summary][]byte{}
)

func statusColor(s agentstatus.Summary) color.NRGBA {
	switch s {
	case agentstatus.SummaryOK:
		return color.NRGBA{R: 0x2f, G: 0xb3, B: 0x6b, A: 0xff}
	case agentstatus.SummaryDegraded:
		return color.NRGBA{R: 0xe6, G: 0xa2, B: 0x17, A: 0xff}
	case agentstatus.SummaryFailed:
		return color.NRGBA{R: 0xd9, G: 0x3d, B: 0x3d, A: 0xff}
	default:
		return color.NRGBA{R: 0x8a, G: 0x8f, B: 0x98, A: 0xff}
	}
}

// StatusIcon returns the PNG tray icon for a contributor summary.
func StatusIcon(s agentstatus.Summary) []byte {
	iconMu.Lock()
	defer iconMu.Unlock()
	if b, ok := iconCache[s]; ok {
		return b
	}
	b := drawRing(64, statusColor(s))
	iconCache[s] = b
	return b
}

// drawRing renders an anti-aliased ring with a centre dot.
func drawRing(size int, c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	center := float64(size) / 2
	outer := center - 2
	inner := outer * 0.62
	dot := outer * 0.28
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)+0.5-center, float64(y)+0.5-center)
			cov := clamp(outer+0.5-d) * clamp(d-inner+0.5)
			cov = math.Max(cov, clamp(dot+0.5-d))
			if cov > 0 {
				img.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: uint8(float64(c.A) * cov)})
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }
