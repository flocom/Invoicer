package brand

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestReadable(t *testing.T) {
	for _, c := range []string{"#ffeb3b", "#00e5ff", "#ff9800", "#a3e635", "#ffffff", "#4338ca", "#16a34a", "nope"} {
		r := Readable(c)
		if Contrast(r, "#ffffff") < 4.5 {
			t.Errorf("%s → %s has contrast %.2f", c, r, Contrast(r, "#ffffff"))
		}
	}
	if Readable("#4338ca") != "#4338ca" {
		t.Error("an already readable colour must not change")
	}
}

func TestFromLogo(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 100, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 100; x++ {
			switch {
			case x < 30:
				img.Set(x, y, color.NRGBA{0xe1, 0x1d, 0x48, 0xff}) // brand red
			case x < 60:
				img.Set(x, y, color.NRGBA{0x11, 0x11, 0x11, 0xff}) // black text
			default:
				img.Set(x, y, color.NRGBA{0, 0, 0, 0}) // transparent
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	got, ok := FromLogo(buf.Bytes())
	if !ok || got != "#e11d48" {
		t.Fatalf("got %s %v", got, ok)
	}
	gray := image.NewGray(image.Rect(0, 0, 10, 10))
	buf.Reset()
	png.Encode(&buf, gray)
	if _, ok := FromLogo(buf.Bytes()); ok {
		t.Fatal("monochrome logos have no brand colour")
	}
}
