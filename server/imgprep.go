package main

// Příprava dokladu pro čtení: dlouhá úzká účtenka by se u OpenAI i Anthropicu
// zmenšila tak, že písmo je nečitelné (čísla, množství). Server ji proto
// rozřeže na několik navazujících částí – řezy vede v bílých mezerách mezi
// řádky, aby se žádný řádek nerozdělil ani nezopakoval. Skenované PDF (jen
// jedna fotka, bez textu – např. z CamScanneru) se čte jako ta fotka.

import (
	"bytes"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// pdfScanJPEG returns the photo inside a scanned one-page PDF: no fonts (no
// text layer) and a single large JPEG image. Anything else stays a PDF.
func pdfScanJPEG(pdf []byte) []byte {
	if bytes.Contains(pdf, []byte("/Font")) {
		return nil
	}
	var found [][]byte
	for i := 0; ; {
		j := bytes.Index(pdf[i:], []byte("/DCTDecode"))
		if j < 0 {
			break
		}
		i += j + 10
		k := bytes.Index(pdf[i:], []byte("stream"))
		if k < 0 {
			break
		}
		start := i + k + 6
		for start < len(pdf) && (pdf[start] == '\r' || pdf[start] == '\n') {
			start++
		}
		end := bytes.Index(pdf[start:], []byte("endstream"))
		if end < 0 {
			break
		}
		data := pdf[start : start+end]
		if e := bytes.LastIndex(data, []byte{0xFF, 0xD9}); e > 0 {
			data = data[:e+2]
		}
		if len(data) > 50<<10 && bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
			found = append(found, data)
		}
		i = start + end
	}
	if len(found) != 1 {
		return nil
	}
	return found[0]
}

const (
	tileMaxRatio = 1.05 // height / width of one part
	tileMaxWidth = 1600 // wider photos are made smaller first
	maxTiles     = 8
)

// prepImage turns one photo into the parts the model gets; a photo that is
// not tall is passed through unchanged.
func prepImage(in aiInput) []aiInput {
	img, _, err := image.Decode(bytes.NewReader(in.data))
	if err != nil {
		return []aiInput{in}
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || float64(h)/float64(w) <= 1.2 {
		return []aiInput{in}
	}
	n := int(math.Ceil(float64(h) / (float64(w) * tileMaxRatio)))
	if n > maxTiles {
		n = maxTiles
	}
	gray := rowBrightness(img)
	cuts := []int{0}
	for k := 1; k < n; k++ {
		target := h * k / n
		cuts = append(cuts, whitestRow(gray, target, h/(n*5)+1, cuts[len(cuts)-1]+h/(n*3)))
	}
	cuts = append(cuts, h)
	var out []aiInput
	for k := 0; k+1 < len(cuts); k++ {
		r := image.Rect(b.Min.X, b.Min.Y+cuts[k], b.Max.X, b.Min.Y+cuts[k+1])
		tile := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
		draw.Draw(tile, tile.Bounds(), img, r.Min, draw.Src)
		var buf bytes.Buffer
		if jpeg.Encode(&buf, scaleDown(tile, tileMaxWidth), &jpeg.Options{Quality: 88}) != nil {
			return []aiInput{in}
		}
		out = append(out, aiInput{mime: "image/jpeg", data: buf.Bytes(), name: in.name})
	}
	return out
}

// rowBrightness: how light each pixel row is (the paper between text lines is
// lightest and most even).
func rowBrightness(img image.Image) []float64 {
	b := img.Bounds()
	rows := make([]float64, b.Dy())
	step := b.Dx()/300 + 1
	for y := 0; y < b.Dy(); y++ {
		sum, dark, n := 0.0, 0, 0
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, _ := img.At(x, b.Min.Y+y).RGBA()
			v := float64(r+g+bl) / 3 / 65535
			sum += v
			if v < .55 {
				dark++
			}
			n++
		}
		rows[y] = sum/float64(n) - 3*float64(dark)/float64(n) // a row crossing text gets a big penalty
	}
	return rows
}

// whitestRow picks the cut nearest to target (within ±win) where a band of a
// few rows is the lightest; never above min.
func whitestRow(rows []float64, target, win, min int) int {
	best, bestV := target, -1e9
	for y := target - win; y <= target+win; y++ {
		if y <= min || y < 3 || y > len(rows)-4 {
			continue
		}
		v := 0.0
		for d := -3; d <= 3; d++ {
			v += rows[y+d]
		}
		v -= math.Abs(float64(y-target)) / float64(win) * .3 // prefer close to the target
		if v > bestV {
			best, bestV = y, v
		}
	}
	return best
}

func scaleDown(img image.Image, maxW int) image.Image {
	b := img.Bounds()
	if b.Dx() <= maxW {
		return img
	}
	dst := image.NewRGBA(image.Rect(0, 0, maxW, b.Dy()*maxW/b.Dx()))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

// docParts: what goes to the model for one uploaded document.
func docParts(in aiInput) []aiInput {
	if in.mime == "application/pdf" {
		if j := pdfScanJPEG(in.data); j != nil {
			return prepImage(aiInput{mime: "image/jpeg", data: j, name: in.name})
		}
		return []aiInput{in}
	}
	return prepImage(in)
}
