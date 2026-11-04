package favicon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
)

// Limits on an image taken as an icon, and the icon's size.
const (
	MaxBytes = 256 << 10
	MaxSide  = 256
	Size     = 64
)

var (
	pngMagic = []byte("\x89PNG\r\n\x1a\n")
	icoMagic = []byte{0, 0, 1, 0}
)

// Sanitize decodes a PNG, or an ICO holding PNG or 24/32-bit BMP images,
// of at most MaxBytes and MaxSide×MaxSide, and draws it again as a
// Size×Size PNG, centred on transparency if it isn't square. Nothing of
// the original file but its pixels survives.
func Sanitize(blob []byte) ([]byte, error) {
	if len(blob) > MaxBytes {
		return nil, fmt.Errorf("the image is over %d KiB", MaxBytes>>10)
	}
	var img image.Image
	var err error
	switch {
	case bytes.HasPrefix(blob, pngMagic):
		img, err = decodePNG(blob)
	case bytes.HasPrefix(blob, icoMagic):
		img, err = decodeICO(blob)
	default:
		return nil, errors.New("not a PNG or ICO image")
	}
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, scale(img, Size)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// decodePNG checks the size the header claims before decoding the pixels.
func decodePNG(b []byte) (image.Image, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if err := checkSide(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(b))
}

func checkSide(w, h int) error {
	if w <= 0 || h <= 0 || w > MaxSide || h > MaxSide {
		return fmt.Errorf("the image is %d×%d; at most %d×%d is taken", w, h, MaxSide, MaxSide)
	}
	return nil
}

// decodeICO decodes the best of an ICO's images: the largest up to Size,
// else the smallest above it.
func decodeICO(b []byte) (image.Image, error) {
	if len(b) < 6 {
		return nil, errors.New("ico: too short")
	}
	n := int(binary.LittleEndian.Uint16(b[4:]))
	if n == 0 || n > 64 || len(b) < 6+16*n {
		return nil, errors.New("ico: bad directory")
	}
	type entry struct {
		side       int
		start, end int
	}
	var entries []entry
	for i := range n {
		e := b[6+16*i:]
		w, h := int(e[0]), int(e[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		size := int64(binary.LittleEndian.Uint32(e[8:]))
		off := int64(binary.LittleEndian.Uint32(e[12:]))
		if off < int64(6+16*n) || size <= 0 || off+size > int64(len(b)) {
			continue
		}
		entries = append(entries, entry{side: max(w, h), start: int(off), end: int(off + size)})
	}
	better := func(a, b entry) bool {
		switch {
		case a.side <= Size && b.side <= Size:
			return a.side > b.side
		case a.side <= Size || b.side <= Size:
			return a.side <= Size
		default:
			return a.side < b.side
		}
	}
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && better(entries[j], entries[j-1]); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	err := errors.New("ico: no image in it")
	for _, e := range entries {
		data := b[e.start:e.end]
		var img image.Image
		if bytes.HasPrefix(data, pngMagic) {
			img, err = decodePNG(data)
		} else {
			img, err = decodeDIB(data)
		}
		if err == nil {
			return img, nil
		}
	}
	return nil, err
}

// decodeDIB decodes an ICO's BMP image: a BITMAPINFOHEADER with twice the
// height, uncompressed 32-bit BGRA or 24-bit BGR rows from the bottom up,
// then a 1-bit transparency mask.
func decodeDIB(b []byte) (image.Image, error) {
	if len(b) < 40 {
		return nil, errors.New("ico: bmp too short")
	}
	hdr := int(binary.LittleEndian.Uint32(b))
	w := int(int32(binary.LittleEndian.Uint32(b[4:])))
	h := int(int32(binary.LittleEndian.Uint32(b[8:]))) / 2
	bpp := int(binary.LittleEndian.Uint16(b[14:]))
	compression := binary.LittleEndian.Uint32(b[16:])
	if hdr < 40 || hdr > len(b) || compression != 0 || (bpp != 32 && bpp != 24) {
		return nil, fmt.Errorf("ico: a %d-bit or compressed bmp isn't taken", bpp)
	}
	if err := checkSide(w, h); err != nil {
		return nil, err
	}
	stride := (w*bpp/8 + 3) &^ 3
	maskStride := ((w+7)/8 + 3) &^ 3
	pix := b[hdr:]
	if len(pix) < stride*h {
		return nil, errors.New("ico: bmp cut short")
	}
	mask := pix[stride*h:]
	hasMask := len(mask) >= maskStride*h
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	anyAlpha := false
	for y := range h {
		row := pix[(h-1-y)*stride:]
		for x := range w {
			p := row[x*bpp/8:]
			c := color.NRGBA{R: p[2], G: p[1], B: p[0], A: 255}
			if bpp == 32 {
				c.A = p[3]
				anyAlpha = anyAlpha || c.A != 0
			}
			img.SetNRGBA(x, y, c)
		}
	}
	// 24-bit images, and 32-bit ones with no alpha at all, use the mask.
	if (bpp == 24 || !anyAlpha) && hasMask {
		for y := range h {
			row := mask[(h-1-y)*maskStride:]
			for x := range w {
				transparent := row[x/8]&(0x80>>(x%8)) != 0
				c := img.NRGBAAt(x, y)
				if transparent {
					c.A = 0
				} else {
					c.A = 255
				}
				img.SetNRGBA(x, y, c)
			}
		}
	} else if !anyAlpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	return img, nil
}

// scale draws src into a size×size image, keeping its proportions, by
// averaging the source area under each pixel (alpha premultiplied, so
// transparent pixels don't darken the edges).
func scale(src image.Image, size int) *image.RGBA {
	b := src.Bounds()
	in := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(in, in.Bounds(), src, b.Min, draw.Src)
	sw, sh := b.Dx(), b.Dy()
	f := float64(size) / float64(max(sw, sh))
	dw, dh := max(1, int(float64(sw)*f+0.5)), max(1, int(float64(sh)*f+0.5))
	ox, oy := (size-dw)/2, (size-dh)/2
	out := image.NewRGBA(image.Rect(0, 0, size, size))
	fx, fy := float64(sw)/float64(dw), float64(sh)/float64(dh)
	for y := range dh {
		y0, y1 := float64(y)*fy, float64(y+1)*fy
		for x := range dw {
			x0, x1 := float64(x)*fx, float64(x+1)*fx
			var r, g, bl, a, area float64
			for sy := int(y0); sy < sh && float64(sy) < y1; sy++ {
				wy := min(y1, float64(sy+1)) - max(y0, float64(sy))
				for sx := int(x0); sx < sw && float64(sx) < x1; sx++ {
					wx := min(x1, float64(sx+1)) - max(x0, float64(sx))
					w := wx * wy
					i := in.PixOffset(sx, sy)
					p := in.Pix[i : i+4 : i+4]
					r += w * float64(p[0])
					g += w * float64(p[1])
					bl += w * float64(p[2])
					a += w * float64(p[3])
					area += w
				}
			}
			if area == 0 {
				continue
			}
			i := out.PixOffset(ox+x, oy+y)
			out.Pix[i+0] = uint8(r/area + 0.5)
			out.Pix[i+1] = uint8(g/area + 0.5)
			out.Pix[i+2] = uint8(bl/area + 0.5)
			out.Pix[i+3] = uint8(a/area + 0.5)
		}
	}
	return out
}

// WriteFile saves png as dir's icon (FileName), replacing any old one in
// one step, so a reader sees either.
func WriteFile(dir string, png []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	const tmp = "." + FileName + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(png); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, FileName)
}

// RemoveFile deletes dir's icon, if it has one.
func RemoveFile(dir string) error {
	err := os.Remove(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
