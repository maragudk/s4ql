// Command siteicons draws the favicons of the project site from its logo, and writes them with the web
// app manifest into the directory of -out, site/ by default. Run it with make site-icons.
//
// The logo is the mark in the headers of the site's pages: the bits of [word], most significant first,
// as white bars on black, one column a bit and [rows] columns high. A change of the logo is a change of
// those two, and a run of this command. Each raster icon is drawn for its own pixel grid, as its
// [layout] says, so that every bar is a whole number of pixels wide and no pixel is grey. At 16 pixels
// the 29 columns of the ink do not fit with a margin, so that size shows the first two bytes only.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// word is the logo: its bits, most significant first, are the bars.
const word = "s4ql"

// rows is the height of the bars, in bits, as in the mark of the pages' headers.
const rows = 22

var (
	black = color.RGBA{A: 0xff}
	white = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
)

// run is a bar: the bits from start to end, end not included, are set.
type run struct{ start, end int }

// layout of a raster icon of size by size pixels: the first bytes of [word] as bars, each bit unit
// pixels wide and the bars rows times unit pixels high, centred on the ink on an opaque black square.
// A maskable icon holds its ink inside the safe zone of the web app manifest's maskable icons, a
// circle of 40% of the size in radius.
type layout struct {
	size, bytes, unit, rows int
	maskable                bool
}

// layouts of every raster icon of the set.
var layouts = []layout{
	{size: 16, bytes: 2, unit: 1, rows: 10},
	{size: 32, bytes: 4, unit: 1, rows: rows},
	{size: 48, bytes: 4, unit: 1, rows: rows},
	{size: 96, bytes: 4, unit: 2, rows: rows},
	{size: 180, bytes: 4, unit: 4, rows: rows},
	{size: 192, bytes: 4, unit: 4, rows: rows},
	{size: 512, bytes: 4, unit: 10, rows: rows, maskable: true},
}

// layoutOf the given size in [layouts].
func layoutOf(size int) layout {
	for _, l := range layouts {
		if l.size == size {
			return l
		}
	}
	panic(fmt.Sprint("no layout of size ", size))
}

// bars of the bits of s, most significant bit of each byte first.
func bars(s string) []run {
	var rs []run
	for i := range len(s) * 8 {
		if s[i/8]&(0x80>>(i%8)) == 0 {
			continue
		}
		if n := len(rs); n > 0 && rs[n-1].end == i {
			rs[n-1].end = i + 1
			continue
		}
		rs = append(rs, run{i, i + 1})
	}
	return rs
}

// rects of the bars of l, in pixels.
func rects(l layout) []image.Rectangle {
	rs := bars(word[:l.bytes])
	if len(rs) == 0 {
		return nil
	}
	first, last := rs[0].start, rs[len(rs)-1].end
	x0 := (l.size-(last-first)*l.unit)/2 - first*l.unit
	y0 := (l.size - l.rows*l.unit) / 2
	var out []image.Rectangle
	for _, r := range rs {
		out = append(out, image.Rect(x0+r.start*l.unit, y0, x0+r.end*l.unit, y0+l.rows*l.unit))
	}
	return out
}

// draw the icon of l.
func draw(l layout) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, l.size, l.size))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xff // opaque black
	}
	for _, r := range rects(l) {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.SetRGBA(x, y, white)
			}
		}
	}
	return img
}

// encode img as a PNG.
func encode(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	e := png.Encoder{CompressionLevel: png.BestCompression}
	if err := e.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ico of the given square images, each stored as a PNG, which every browser that reads ICO reads.
func ico(imgs ...*image.RGBA) ([]byte, error) {
	var head, data bytes.Buffer
	// ICONDIR: reserved, type 1 for an icon, the number of images.
	_ = binary.Write(&head, binary.LittleEndian, [3]uint16{0, 1, uint16(len(imgs))})
	offset := 6 + 16*len(imgs)
	for _, img := range imgs {
		p, err := encode(img)
		if err != nil {
			return nil, err
		}
		size := img.Bounds().Dx()
		// ICONDIRENTRY: width and height, where 0 means 256, colours, reserved, colour planes, bits a
		// pixel, the length of the image, its offset from the start of the file.
		head.Write([]byte{byte(size), byte(size), 0, 0})
		_ = binary.Write(&head, binary.LittleEndian, struct {
			Planes, BitCount uint16
			Length, Offset   uint32
		}{1, 32, uint32(len(p)), uint32(offset + data.Len())})
		data.Write(p)
	}
	return append(head.Bytes(), data.Bytes()...), nil
}

// svg of the icons of 32 and 16 pixels, as a vector on an opaque black square, so that it reads on a
// light tab and on a dark one. The icon of 32 pixels is drawn at its size, and that of 16 at twice its
// size. A media query in the SVG, which a browser evaluates for the size at which it draws the image,
// shows that of 16 pixels at up to 24 CSS pixels, where the bars of a pixel each of the other would
// fall between the pixels and blur, as in a tab, and that of 32 pixels above.
func svg() []byte {
	path := func(l layout, scale int) string {
		var d strings.Builder
		for _, r := range rects(l) {
			fmt.Fprintf(&d, "M%d %dh%dv%dh-%dz", r.Min.X*scale, r.Min.Y*scale, r.Dx()*scale, r.Dy()*scale, r.Dx()*scale)
		}
		return d.String()
	}
	return fmt.Appendf(nil, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" shape-rendering="crispEdges">`+
		`<style>.small{display:none}@media (max-width:24px){.small{display:inline}.large{display:none}}</style>`+
		`<rect width="32" height="32" fill="#000"/><path class="large" d="%s" fill="#fff"/><path class="small" d="%s" fill="#fff"/></svg>`+"\n",
		path(layoutOf(32), 1), path(layoutOf(16), 2))
}

// manifest is the web app manifest, with the icons of 192 and 512 pixels, each with the purpose any,
// and a maskable icon also as maskable. That is an entry of its own, since a browser may warn about
// one entry for both purposes.
func manifest() ([]byte, error) {
	type icon struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	}
	m := struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		Icons           []icon `json:"icons"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Display         string `json:"display"`
	}{Name: "s4ql", ShortName: "s4ql", ThemeColor: "#000000", BackgroundColor: "#000000", Display: "standalone"}
	for _, size := range []int{192, 512} {
		add := func(purpose string) {
			m.Icons = append(m.Icons, icon{Src: fmt.Sprintf("web-app-manifest-%[1]dx%[1]d.png", size), Sizes: fmt.Sprintf("%[1]dx%[1]d", size), Type: "image/png", Purpose: purpose})
		}
		add("any")
		if layoutOf(size).maskable {
			add("maskable")
		}
	}
	b, err := json.Marshal(m, jsontext.Multiline(true), jsontext.WithIndent("  "))
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// write every file of the set into dir.
func write(dir string) error {
	files := map[string][]byte{"favicon.svg": svg()}

	f, err := ico(draw(layoutOf(16)), draw(layoutOf(32)), draw(layoutOf(48)))
	if err != nil {
		return err
	}
	files["favicon.ico"] = f

	for name, size := range map[string]int{
		"favicon-96x96.png":            96,
		"apple-touch-icon.png":         180,
		"web-app-manifest-192x192.png": 192,
		"web-app-manifest-512x512.png": 512,
	} {
		p, err := encode(draw(layoutOf(size)))
		if err != nil {
			return err
		}
		files[name] = p
	}

	m, err := manifest()
	if err != nil {
		return err
	}
	files["site.webmanifest"] = m

	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	out := flag.String("out", "site", "the directory to write the icons and the manifest into")
	flag.Parse()
	if err := write(*out); err != nil {
		log.Fatalln("siteicons:", err)
	}
}
