package vision

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestScaleMaxWidth(t *testing.T) {
	// 造一张 1000x500 的 PNG
	src := image.NewRGBA(image.Rect(0, 0, 1000, 500))
	for y := 0; y < 500; y++ {
		for x := 0; x < 1000; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}

	out, err := scaleMaxWidth(buf.Bytes(), 640)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode scaled: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 640 {
		t.Errorf("width = %d, want 640", b.Dx())
	}
	wantH := 320
	if b.Dy() != wantH {
		t.Errorf("height = %d, want %d", b.Dy(), wantH)
	}

	// maxWidth 不小于原宽时原样返回
	if got, _ := scaleMaxWidth(buf.Bytes(), 2000); !bytes.Equal(got, buf.Bytes()) {
		t.Error("should return original when maxWidth>=width")
	}
	// maxWidth<=0 原样返回
	if got, _ := scaleMaxWidth(buf.Bytes(), 0); !bytes.Equal(got, buf.Bytes()) {
		t.Error("should return original when maxWidth<=0")
	}
}
