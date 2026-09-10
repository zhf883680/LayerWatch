package vision

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"
)

func scaleMaxWidth(data []byte, maxWidth int) ([]byte, error) {
	if maxWidth <= 0 {
		return data, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, err
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= maxWidth {
		return data, nil
	}
	rgba := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	w := maxWidth
	h := int(math.Round(float64(sh) * float64(w) / float64(sw)))
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5
		y0 := int(math.Floor(sy))
		y1 := y0 + 1
		dy := sy - float64(y0)
		if y0 < 0 {
			y0 = 0
		}
		if y1 >= sh {
			y1 = sh - 1
		}
		for x := 0; x < w; x++ {
			sx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5
			x0 := int(math.Floor(sx))
			x1 := x0 + 1
			dx := sx - float64(x0)
			if x0 < 0 {
				x0 = 0
			}
			if x1 >= sw {
				x1 = sw - 1
			}
			c00, c10 := rgba.RGBAAt(x0, y0), rgba.RGBAAt(x1, y0)
			c01, c11 := rgba.RGBAAt(x0, y1), rgba.RGBAAt(x1, y1)
			r := lerp(lerp(float64(c00.R), float64(c10.R), dx), lerp(float64(c01.R), float64(c11.R), dx), dy)
			g := lerp(lerp(float64(c00.G), float64(c10.G), dx), lerp(float64(c01.G), float64(c11.G), dx), dy)
			bl := lerp(lerp(float64(c00.B), float64(c10.B), dx), lerp(float64(c01.B), float64(c11.B), dx), dy)
			a := lerp(lerp(float64(c00.A), float64(c10.A), dx), lerp(float64(c01.A), float64(c11.A), dx), dy)
			dst.SetRGBA(x, y, color.RGBA{R: clamp8(r), G: clamp8(g), B: clamp8(bl), A: clamp8(a)})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return data, err
	}
	return out.Bytes(), nil
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }
func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(math.Round(v))
}
