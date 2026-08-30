package web

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"io/fs"
	"testing"
)

// The icons live in web/public and reach dist only because Vite copies that
// directory. Nothing else notices when they do not, so this does: a build that
// drops them ships a binary with no favicon and no social card.
func TestIconsAreEmbedded(t *testing.T) {
	dist, err := Dist()
	if err != nil {
		t.Fatal(err)
	}

	pngs := map[string][2]int{
		"apple-touch-icon.png": {180, 180},
		"icon-192.png":         {192, 192},
		"icon-512.png":         {512, 512},
		// Open Graph wants 1200x630; an unfurler crops anything else.
		"og-image.png": {1200, 630},
	}
	for name, want := range pngs {
		b, err := fs.ReadFile(dist, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s is not a PNG: %v", name, err)
			continue
		}
		if cfg.Width != want[0] || cfg.Height != want[1] {
			t.Errorf("%s is %dx%d, want %dx%d", name, cfg.Width, cfg.Height, want[0], want[1])
		}
	}

	for _, name := range []string{"favicon.svg", "site.webmanifest"} {
		if b, err := fs.ReadFile(dist, name); err != nil || len(b) == 0 {
			t.Errorf("%s: %v (%d bytes)", name, err, len(b))
		}
	}
}

// A truncated or mislabelled .ico is served happily and drawn by nobody, so
// the header is checked rather than the file's mere existence.
func TestFaviconICO(t *testing.T) {
	dist, err := Dist()
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(dist, "favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 6 {
		t.Fatalf("favicon.ico is %d bytes", len(b))
	}

	reserved := binary.LittleEndian.Uint16(b[0:2])
	kind := binary.LittleEndian.Uint16(b[2:4])
	count := binary.LittleEndian.Uint16(b[4:6])
	if reserved != 0 || kind != 1 {
		t.Fatalf("ICONDIR = (%d, %d), want (0, 1)", reserved, kind)
	}
	if count == 0 {
		t.Fatal("favicon.ico carries no images")
	}

	// 16px is the size a browser tab actually draws, so it has to be present.
	var sizes []int
	for i := range int(count) {
		e := b[6+16*i:]
		if len(e) < 16 {
			t.Fatalf("entry %d is truncated", i)
		}
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		sizes = append(sizes, w)

		length := binary.LittleEndian.Uint32(e[8:12])
		offset := binary.LittleEndian.Uint32(e[12:16])
		if int(offset+length) > len(b) {
			t.Errorf("entry %d points past the end of the file", i)
		}
	}
	if !contains(sizes, 16) {
		t.Errorf("sizes = %v, want a 16x16 among them", sizes)
	}
}

func contains(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
