package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"strings"
	"testing"
)

// a tall "receipt": dark text lines 20 px high with 12 px gaps
func tallReceipt(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(1))
	for y := 0; y < h; y++ {
		text := y%32 < 20
		for x := 0; x < w; x++ {
			c := uint8(245)
			if text && rnd.Intn(3) == 0 {
				c = 20
			}
			img.Set(x, y, color.Gray{c})
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	return b.Bytes()
}

func TestPrepImageCutsInGaps(t *testing.T) {
	parts := prepImage(aiInput{mime: "image/jpeg", data: tallReceipt(400, 1600)})
	if len(parts) < 3 {
		t.Fatalf("parts %d", len(parts))
	}
	total := 0
	for i, p := range parts {
		img, err := jpeg.Decode(bytes.NewReader(p.data))
		if err != nil {
			t.Fatal(err)
		}
		h := img.Bounds().Dy()
		if i > 0 && total%32 < 20 { // the cut must not be inside a text line
			t.Fatalf("cut at %d is in a text line", total)
		}
		total += h
	}
	if total != 1600 {
		t.Fatalf("parts add up to %d", total)
	}
	if one := prepImage(aiInput{mime: "image/jpeg", data: tallReceipt(400, 420)}); len(one) != 1 {
		t.Fatalf("short image cut into %d", len(one))
	}
}

func TestPdfScanJPEG(t *testing.T) {
	j := tallReceipt(400, 1600)
	pdf := []byte("%PDF-1.3\n1 0 obj << /Type /XObject /Subtype /Image /Filter /DCTDecode /Length 1 >>\nstream\n")
	pdf = append(pdf, j...)
	pdf = append(pdf, []byte("\nendstream\nendobj\n%%EOF")...)
	if got := pdfScanJPEG(pdf); !bytes.Equal(got, j) {
		t.Fatalf("scan image not found (%d bytes)", len(got))
	}
	if pdfScanJPEG(append([]byte("/Font "), pdf...)) != nil {
		t.Fatal("a PDF with text must stay a PDF")
	}
	if parts := docParts(aiInput{mime: "application/pdf", data: pdf}); len(parts) < 2 || parts[0].mime != "image/jpeg" {
		t.Fatalf("parts %d", len(parts))
	}
}

func TestCleanAiDocFixesQuantities(t *testing.T) {
	d := aiDoc{Total: 1205, Items: []aiItem{
		{Name: "Lepidlo", Qty: 1, Price: 139, Total: 695}, // 5 ks à 139 read as 1 ks
		{Name: "Kout", Qty: 8, Price: 392, Total: 392},    // line total taken as unit price
		{Name: "Taška", Qty: 0, Price: 9},                 // nothing but a price
		{Name: "Lišta", Qty: 1, Total: 109},
	}}
	cleanAiDoc(&d)
	it := d.Items
	if it[0].Qty != 5 || it[1].Price != 49 || it[2].Qty != 1 || it[2].Total != 9 || it[3].Price != 109 {
		t.Fatalf("items %+v", it)
	}
	if d.Note != "" {
		t.Fatalf("sum fits, no note expected: %q", d.Note)
	}
	d.Total = 2000
	d.Note = ""
	cleanAiDoc(&d)
	if !strings.Contains(d.Note, "nesedí") {
		t.Fatalf("note %q", d.Note)
	}
}
