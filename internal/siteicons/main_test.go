package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json/v2"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"maragu.dev/is"
)

func TestBars(t *testing.T) {
	t.Run("should give the runs of set bits of the word, most significant bit first", func(t *testing.T) {
		// 01110011 00110100 01110001 01101100
		is.EqualSlice(t, []run{{1, 4}, {6, 8}, {10, 12}, {13, 14}, {17, 20}, {23, 24}, {25, 27}, {28, 30}}, bars("s4ql"))
	})

	t.Run("should give the runs of the first bytes only", func(t *testing.T) {
		is.EqualSlice(t, []run{{1, 4}, {6, 8}, {10, 12}, {13, 14}}, bars("s4"))
	})

	t.Run("should join a run across the boundary of two bytes", func(t *testing.T) {
		is.EqualSlice(t, []run{{7, 9}}, bars("\x01\x80"))
	})
}

func TestDraw(t *testing.T) {
	for _, l := range layouts {
		t.Run("should draw the bars of its bytes on whole pixels at size "+fmt.Sprint(l.size), func(t *testing.T) {
			img := draw(l)
			is.Equal(t, image.Rect(0, 0, l.size, l.size), img.Bounds())

			// Every pixel is black or white, so no bar is blurred.
			for y := range l.size {
				for x := range l.size {
					c := img.RGBAAt(x, y)
					is.True(t, c == black || c == white, "pixel", x, y, "is", c)
				}
			}

			// The white pixels form whole bars: each column is white from top to bottom or not at all.
			minX, minY, maxX, maxY := l.size, l.size, -1, -1
			for x := range l.size {
				var n int
				for y := range l.size {
					if img.RGBAAt(x, y) == white {
						n++
						minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
					}
				}
				is.True(t, n == 0 || n == l.rows*l.unit, "column", x, "has", n, "white pixels")
			}

			// The middle row reads as the bars of the bytes, each bit l.unit pixels wide.
			rs := bars(word[:l.bytes])
			var got []run
			y := l.size / 2
			for x := 0; x < l.size; x++ {
				if img.RGBAAt(x, y) != white {
					continue
				}
				start := x
				for x < l.size && img.RGBAAt(x, y) == white {
					x++
				}
				got = append(got, run{start, x})
			}
			is.Equal(t, len(rs), len(got))
			for i := range rs {
				is.Equal(t, (rs[i].end-rs[i].start)*l.unit, got[i].end-got[i].start, "bar", i)
			}

			// The ink is centred, within a pixel, with a margin of at least one pixel.
			is.True(t, minX >= 1 && minY >= 1 && maxX <= l.size-2 && maxY <= l.size-2, "the ink touches the edge")
			is.True(t, abs(minX-(l.size-1-maxX)) <= 1, "the ink is not centred across")
			is.True(t, abs(minY-(l.size-1-maxY)) <= 1, "the ink is not centred down")
		})
	}

	t.Run("should keep the ink of a maskable icon inside the safe zone", func(t *testing.T) {
		var n int
		for _, l := range layouts {
			if !l.maskable {
				continue
			}
			n++
			img := draw(l)
			centre, radius := float64(l.size)/2, 0.4*float64(l.size)
			for y := range l.size {
				for x := range l.size {
					if img.RGBAAt(x, y) != white {
						continue
					}
					// The corner of the pixel farthest from the centre.
					dx := math.Max(math.Abs(float64(x)-centre), math.Abs(float64(x+1)-centre))
					dy := math.Max(math.Abs(float64(y)-centre), math.Abs(float64(y+1)-centre))
					is.True(t, math.Hypot(dx, dy) <= radius, "pixel", x, y, "of size", l.size, "is outside the safe zone")
				}
			}
		}
		is.True(t, n > 0, "no layout is maskable")
	})
}

func TestICO(t *testing.T) {
	t.Run("should hold a PNG image of each size, which decodes to that size", func(t *testing.T) {
		b, err := ico(draw(layoutOf(16)), draw(layoutOf(32)), draw(layoutOf(48)))
		is.NotError(t, err)

		is.Equal(t, uint16(0), binary.LittleEndian.Uint16(b[0:]))
		is.Equal(t, uint16(1), binary.LittleEndian.Uint16(b[2:]))
		is.Equal(t, uint16(3), binary.LittleEndian.Uint16(b[4:]))
		for i, size := range []int{16, 32, 48} {
			e := b[6+16*i:]
			is.Equal(t, byte(size), e[0])
			is.Equal(t, byte(size), e[1])
			is.Equal(t, uint16(1), binary.LittleEndian.Uint16(e[4:]))
			is.Equal(t, uint16(32), binary.LittleEndian.Uint16(e[6:]))
			n, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
			is.True(t, int(off+n) <= len(b))
			img, err := png.Decode(bytes.NewReader(b[off : off+n]))
			is.NotError(t, err)
			is.Equal(t, image.Rect(0, 0, size, size), img.Bounds())
		}
	})
}

func TestSVG(t *testing.T) {
	t.Run("should parse as SVG with an opaque square, the bars of the 32 pixel icon, and those of the 16 pixel icon for small sizes", func(t *testing.T) {
		var doc struct {
			XMLName xml.Name `xml:"http://www.w3.org/2000/svg svg"`
			ViewBox string   `xml:"viewBox,attr"`
			Style   string   `xml:"style"`
			Rect    struct {
				Width  string `xml:"width,attr"`
				Height string `xml:"height,attr"`
				Fill   string `xml:"fill,attr"`
			} `xml:"rect"`
			Paths []struct {
				Class string `xml:"class,attr"`
				D     string `xml:"d,attr"`
				Fill  string `xml:"fill,attr"`
			} `xml:"path"`
		}
		is.NotError(t, xml.Unmarshal(svg(), &doc))
		is.Equal(t, "0 0 32 32", doc.ViewBox)
		is.Equal(t, ".small{display:none}@media (max-width:24px){.small{display:inline}.large{display:none}}", doc.Style)
		is.Equal(t, "32", doc.Rect.Width)
		is.Equal(t, "32", doc.Rect.Height)
		is.Equal(t, "#000", doc.Rect.Fill)
		is.Equal(t, 2, len(doc.Paths))
		is.Equal(t, "large", doc.Paths[0].Class)
		is.Equal(t, "#fff", doc.Paths[0].Fill)
		is.Equal(t, "M1 5h3v22h-3zM6 5h2v22h-2zM10 5h2v22h-2zM13 5h1v22h-1zM17 5h3v22h-3zM23 5h1v22h-1zM25 5h2v22h-2zM28 5h2v22h-2z", doc.Paths[0].D)
		// The 16 pixel icon, at twice its size, so that it sits on the grid of 16 pixels.
		is.Equal(t, "small", doc.Paths[1].Class)
		is.Equal(t, "#fff", doc.Paths[1].Fill)
		is.Equal(t, "M2 6h6v20h-6zM12 6h4v20h-4zM20 6h4v20h-4zM26 6h2v20h-2z", doc.Paths[1].D)
	})
}

func TestManifest(t *testing.T) {
	t.Run("should name the site and list the two icons of the manifest, the one of 512 pixels also as maskable", func(t *testing.T) {
		b, err := manifest()
		is.NotError(t, err)

		var m struct {
			Name            string `json:"name"`
			ShortName       string `json:"short_name"`
			ThemeColor      string `json:"theme_color"`
			BackgroundColor string `json:"background_color"`
			Display         string `json:"display"`
			StartURL        string `json:"start_url"`
			ID              string `json:"id"`
			Icons           []struct {
				Src     string `json:"src"`
				Sizes   string `json:"sizes"`
				Type    string `json:"type"`
				Purpose string `json:"purpose"`
			} `json:"icons"`
		}
		is.NotError(t, json.Unmarshal(b, &m, json.RejectUnknownMembers(true)))
		is.Equal(t, "s4ql", m.Name)
		is.Equal(t, "s4ql", m.ShortName)
		is.Equal(t, "#000000", m.ThemeColor)
		is.Equal(t, "#000000", m.BackgroundColor)
		is.Equal(t, "standalone", m.Display)
		// Both resolve to the directory of the manifest, the site's root, so that an install from any
		// page is one app that opens on the front page.
		is.Equal(t, "./", m.StartURL)
		is.Equal(t, "./", m.ID)
		is.Equal(t, 3, len(m.Icons))
		for i, want := range []struct{ size, purpose string }{{"192", "any"}, {"512", "any"}, {"512", "maskable"}} {
			is.Equal(t, "web-app-manifest-"+want.size+"x"+want.size+".png", m.Icons[i].Src)
			is.Equal(t, want.size+"x"+want.size, m.Icons[i].Sizes)
			is.Equal(t, "image/png", m.Icons[i].Type)
			is.Equal(t, want.purpose, m.Icons[i].Purpose)
		}
		is.True(t, layoutOf(512).maskable)
	})
}

func TestWrite(t *testing.T) {
	t.Run("should write every file of the set, each PNG at its size, the same on every run", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		is.NotError(t, write(a))
		is.NotError(t, write(b))

		names := []string{"favicon.svg", "favicon-96x96.png", "favicon.ico", "apple-touch-icon.png", "web-app-manifest-192x192.png", "web-app-manifest-512x512.png", "site.webmanifest"}
		entries, err := os.ReadDir(a)
		is.NotError(t, err)
		is.Equal(t, len(names), len(entries))
		for _, name := range names {
			x, err := os.ReadFile(filepath.Join(a, name))
			is.NotError(t, err)
			y, err := os.ReadFile(filepath.Join(b, name))
			is.NotError(t, err)
			is.True(t, bytes.Equal(x, y), name, "differs between two runs")
		}

		for name, size := range map[string]int{"favicon-96x96.png": 96, "apple-touch-icon.png": 180, "web-app-manifest-192x192.png": 192, "web-app-manifest-512x512.png": 512} {
			f, err := os.Open(filepath.Join(a, name))
			is.NotError(t, err)
			img, err := png.Decode(f)
			_ = f.Close()
			is.NotError(t, err)
			is.Equal(t, image.Rect(0, 0, size, size), img.Bounds(), name)
			is.Equal(t, black, color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA), name)
		}
	})
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
