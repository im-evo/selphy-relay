package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
)

type urfPage struct {
	img     *image.RGBA
	w, h    int
	dpi     uint32
	bpp     int
	quality byte
}

func (p urfPage) String() string {
	return fmt.Sprintf("%dx%d @%ddpi %dbpp quality %d", p.w, p.h, p.dpi, p.bpp, p.quality)
}

// decodeURF decodes every page of an Apple URF (UNIRAST) document.
func decodeURF(d []byte) ([]urfPage, error) {
	if len(d) < 12 || string(d[:8]) != "UNIRAST\x00" {
		return nil, errors.New("not a UNIRAST document")
	}
	declared := int(binary.BigEndian.Uint32(d[8:12]))
	i := 12
	var pages []urfPage
	for i < len(d) {
		if len(d) < i+32 {
			return pages, fmt.Errorf("truncated header of page %d", len(pages)+1)
		}
		h := d[i : i+32]
		i += 32
		p := urfPage{bpp: int(h[0]), quality: h[3],
			w:   int(binary.BigEndian.Uint32(h[12:16])),
			h:   int(binary.BigEndian.Uint32(h[16:20])),
			dpi: binary.BigEndian.Uint32(h[20:24])}
		if p.bpp != 24 && p.bpp != 8 {
			return pages, fmt.Errorf("page %d: unsupported bpp %d", len(pages)+1, p.bpp)
		}
		if p.w <= 0 || p.h <= 0 || p.w > 20000 || p.h > 20000 {
			return pages, fmt.Errorf("page %d: bad dimensions %dx%d", len(pages)+1, p.w, p.h)
		}
		px := p.bpp / 8
		line := p.w * px
		p.img = image.NewRGBA(image.Rect(0, 0, p.w, p.h))
		buf := make([]byte, line)
		y := 0
		for y < p.h {
			if i >= len(d) {
				return pages, fmt.Errorf("page %d: data ends at line %d of %d", len(pages)+1, y, p.h)
			}
			rep := int(d[i]) + 1
			i++
			x := 0
			for x < line {
				if i >= len(d) {
					return pages, errors.New("truncated line data")
				}
				n := int(d[i])
				i++
				switch {
				case n == 0x80: // rest of line white
					for ; x < line; x++ {
						buf[x] = 0xff
					}
				case n < 0x80: // repeat next pixel n+1 times
					if i+px > len(d) {
						return pages, errors.New("truncated repeat pixel")
					}
					for k := 0; k <= n && x+px <= line; k++ {
						copy(buf[x:], d[i:i+px])
						x += px
					}
					i += px
				default: // 257-n literal pixels
					cnt := (257 - n) * px
					if i+cnt > len(d) {
						return pages, errors.New("truncated literal run")
					}
					c := cnt
					if x+c > line {
						c = line - x
					}
					copy(buf[x:], d[i:i+c])
					x += c
					i += cnt
				}
			}
			for r := 0; r < rep && y < p.h; r++ {
				off := y * p.img.Stride
				for q := 0; q < p.w; q++ {
					o := off + q*4
					if px == 3 {
						p.img.Pix[o], p.img.Pix[o+1], p.img.Pix[o+2] = buf[q*3], buf[q*3+1], buf[q*3+2]
					} else {
						p.img.Pix[o], p.img.Pix[o+1], p.img.Pix[o+2] = buf[q], buf[q], buf[q]
					}
					p.img.Pix[o+3] = 0xff
				}
				y++
			}
		}
		pages = append(pages, p)
		if declared > 0 && len(pages) == declared {
			break
		}
	}
	if len(pages) == 0 {
		return nil, errors.New("no pages")
	}
	return pages, nil
}

func encodeJPEG(p urfPage, q int) ([]byte, error) {
	var out bytes.Buffer
	err := jpeg.Encode(&out, p.img, &jpeg.Options{Quality: q})
	return out.Bytes(), err
}
