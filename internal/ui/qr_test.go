package ui

import (
	"os"
	"strings"
	"testing"
)

// The matrix in testdata was made once by rsc.io/qr v0.2.0 (this code)
// and checked with an independent reference decoder: ZXing's QR decoder
// (github.com/makiuchi-d/gozxing v0.1.1) read it back as this text with
// zero errors corrected. It was not scanned with a phone (no phone was
// at hand). segno 1.6.6, as a reference encoder, pads the data with one
// more zero byte, so its matrix differs in the padding and check
// codewords; it decodes to the same text.
func TestQRMatrixIsStable(t *testing.T) {
	const text = "otpauth://totp/BrowserPier:pier.example.com?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=BrowserPier"
	want, err := os.ReadFile("testdata/qr-otpauth.txt")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := qrMatrix(text)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rows, "\n") + "\n"; got != string(want) {
		t.Fatalf("QR matrix changed:\n%s", got)
	}
	svg, err := qrSVG(text)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") || strings.Contains(s, "GEZDGNBV") || strings.Contains(s, "otpauth") || !strings.Contains(s, `viewBox="0 0 49 49"`) {
		t.Fatalf("SVG: %.300s", s)
	}
}
