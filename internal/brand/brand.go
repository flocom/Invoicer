// Package brand derives the accent colour of a company from its logo and makes
// sure any accent colour stays readable on invoices and e-mails.
package brand

import (
	"bytes"
	"fmt"
	"image"
	_ "image/png"
	"math"
	"strconv"
)

// Default is used when no usable colour is available.
const Default = "#4338ca"

type rgb struct{ r, g, b float64 } // 0..1

func parse(hex string) (rgb, bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return rgb{}, false
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return rgb{}, false
	}
	return rgb{float64(v>>16&0xff) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}, true
}

func (c rgb) hex() string {
	cl := func(x float64) int { return int(math.Round(math.Max(0, math.Min(1, x)) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", cl(c.r), cl(c.g), cl(c.b))
}

// luminance follows WCAG 2.x relative luminance.
func (c rgb) luminance() float64 {
	lin := func(x float64) float64 {
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

// Contrast returns the WCAG contrast ratio between two colours.
func Contrast(a, b string) float64 {
	ca, ok1 := parse(a)
	cb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return 1
	}
	la, lb := ca.luminance(), cb.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func toHSL(c rgb) (h, s, l float64) {
	mx := math.Max(c.r, math.Max(c.g, c.b))
	mn := math.Min(c.r, math.Min(c.g, c.b))
	l = (mx + mn) / 2
	if mx == mn {
		return 0, 0, l
	}
	d := mx - mn
	if l > 0.5 {
		s = d / (2 - mx - mn)
	} else {
		s = d / (mx + mn)
	}
	switch mx {
	case c.r:
		h = (c.g - c.b) / d
		if c.g < c.b {
			h += 6
		}
	case c.g:
		h = (c.b-c.r)/d + 2
	default:
		h = (c.r-c.g)/d + 4
	}
	return h / 6, s, l
}

func fromHSL(h, s, l float64) rgb {
	if s == 0 {
		return rgb{l, l, l}
	}
	hue := func(p, q, t float64) float64 {
		if t < 0 {
			t++
		}
		if t > 1 {
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	q := l * (1 + s)
	if l >= 0.5 {
		q = l + s - l*s
	}
	p := 2*l - q
	return rgb{hue(p, q, h+1.0/3), hue(p, q, h), hue(p, q, h-1.0/3)}
}

// Readable returns a colour of the same hue that keeps white text readable
// on it and stays readable as text on white (contrast ≥ 4.5:1, WCAG AA). The
// colour is darkened as needed; unusable input falls back to Default.
func Readable(hex string) string {
	c, ok := parse(hex)
	if !ok {
		return Default
	}
	if Contrast(c.hex(), "#ffffff") >= 4.5 {
		return c.hex()
	}
	h, s, l := toHSL(c)
	for l > 0 {
		l -= 0.02
		cand := fromHSL(h, s, math.Max(l, 0)).hex()
		if Contrast(cand, "#ffffff") >= 4.5 {
			return cand
		}
	}
	return Default
}

// FromLogo picks the most representative saturated colour of a logo. It
// ignores transparent, near-white, near-black and grey pixels, and returns
// ("", false) for monochrome logos.
func FromLogo(data []byte) (string, bool) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", false
	}
	b := img.Bounds()
	step := max(1, max(b.Dx(), b.Dy())/200) // sample at most ~200×200 pixels
	type bucket struct {
		weight   float64
		r, g, bl float64
		samples  float64
	}
	buckets := map[int]*bucket{}
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, a := img.At(x, y).RGBA()
			if a < 0x8000 {
				continue
			}
			c := rgb{float64(r) / 0xffff, float64(g) / 0xffff, float64(bl) / 0xffff}
			h, s, l := toHSL(c)
			if s < 0.25 || l < 0.12 || l > 0.92 {
				continue // grey, black or white: not a brand colour
			}
			key := int(h*24)*4 + int(l*4) // 24 hues × 4 lightness bands
			bk := buckets[key]
			if bk == nil {
				bk = &bucket{}
				buckets[key] = bk
			}
			w := s // saturated pixels count more
			bk.weight += w
			bk.r += c.r * w
			bk.g += c.g * w
			bk.bl += c.b * w
			bk.samples++
		}
	}
	var best *bucket
	for _, bk := range buckets {
		if best == nil || bk.weight > best.weight {
			best = bk
		}
	}
	if best == nil || best.samples < 4 {
		return "", false
	}
	return rgb{best.r / best.weight, best.g / best.weight, best.bl / best.weight}.hex(), true
}
