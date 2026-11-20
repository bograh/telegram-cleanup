package telegram

import (
	"testing"

	"rsc.io/qr"
)

func TestQRImageIsScaledWithQuietZone(t *testing.T) {
	c, err := qr.Encode("tg://login?token=abc", qr.M)
	if err != nil {
		t.Fatal(err)
	}
	img := qrImage(c, 8)
	want := (c.Size + 8) * 8
	if b := img.Bounds(); b.Dx() != want || b.Dy() != want {
		t.Fatalf("size %v, want %d", b, want)
	}
	// Quiet zone is white; the top-left finder pattern starts right after it.
	if r, _, _, _ := img.At(0, 0).RGBA(); r != 0xFFFF {
		t.Fatal("quiet zone not white")
	}
	if r, _, _, _ := img.At(4*8, 4*8).RGBA(); r != 0 {
		t.Fatal("finder pattern corner not black")
	}
	// The far corner of the finder pattern (7 modules wide) must also be black,
	// proving modules are scaled rather than drawn as single pixels.
	if r, _, _, _ := img.At(4*8+7*8-1, 4*8).RGBA(); r != 0 {
		t.Fatal("finder pattern not scaled")
	}
}
