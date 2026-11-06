// genicons draws the OpenDM logo procedurally and writes every icon size the
// project needs (PNG for web/extension, multi-size ICO for the .exe).
// Usage: go run ./tools/genicons
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// sdRoundRect returns the signed distance to a rounded rectangle centred at (cx,cy).
func sdRoundRect(x, y, cx, cy, hw, hh, r float64) float64 {
	dx := math.Abs(x-cx) - (hw - r)
	dy := math.Abs(y-cy) - (hh - r)
	ox, oy := math.Max(dx, 0), math.Max(dy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(dx, dy), 0) - r
}

// inside the downward-pointing arrow head triangle
func inTriangle(x, y float64) bool {
	// apex (0.5, 0.70), base from (0.27,0.46) to (0.73,0.46)
	if y < 0.46 || y > 0.70 {
		return false
	}
	half := 0.23 * (0.70 - y) / 0.24
	return math.Abs(x-0.5) <= half
}

func glyph(x, y float64) bool {
	// shaft
	if sdRoundRect(x, y, 0.5, 0.335, 0.07, 0.145, 0.03) <= 0 {
		return true
	}
	if inTriangle(x, y) {
		return true
	}
	// tray / baseline
	return sdRoundRect(x, y, 0.5, 0.79, 0.25, 0.04, 0.04) <= 0
}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 6
	top := [3]float64{99, 102, 241} // indigo
	bot := [3]float64{6, 182, 212}  // cyan
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var bgCov, fgCov float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					if sdRoundRect(x, y, 0.5, 0.5, 0.47, 0.47, 0.22) <= 0 {
						bgCov++
						if glyph(x, y) {
							fgCov++
						}
					}
				}
			}
			n := float64(ss * ss)
			bgCov /= n
			fgCov /= n
			if bgCov == 0 {
				continue
			}
			t := (float64(px) + float64(py)) / float64(2*size)
			r, g, b := lerp(top[0], bot[0], t), lerp(top[1], bot[1], t), lerp(top[2], bot[2], t)
			// white glyph composited over gradient (fgCov is relative to whole pixel)
			f := fgCov / bgCov
			r, g, b = lerp(r, 255, f), lerp(g, 255, f), lerp(b, 255, f)
			img.SetNRGBA(px, py, color.NRGBA{uint8(r), uint8(g), uint8(b), uint8(math.Round(bgCov * 255))})
		}
	}
	return img
}

func pngBytes(size int) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, render(size))
	return buf.Bytes()
}

func writeICO(path string, sizes []int) {
	var imgs [][]byte
	for _, s := range sizes {
		imgs = append(imgs, pngBytes(s))
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := uint8(s)
		if s >= 256 {
			w = 0
		}
		buf.Write([]byte{w, w, 0, 0})
		binary.Write(&buf, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&buf, binary.LittleEndian, []uint32{uint32(len(imgs[i])), uint32(offset)})
		offset += len(imgs[i])
	}
	for _, b := range imgs {
		buf.Write(b)
	}
	must(os.WriteFile(path, buf.Bytes(), 0o644))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	for _, s := range []int{16, 32, 48, 128} {
		must(os.WriteFile(filepath.Join("extension", "icons", "icon"+itoa(s)+".png"), pngBytes(s), 0o644))
	}
	for _, s := range []int{32, 64, 192} {
		must(os.WriteFile(filepath.Join("web", "icon-"+itoa(s)+".png"), pngBytes(s), 0o644))
	}
	writeICO(filepath.Join("assets", "opendm.ico"), []int{16, 20, 24, 32, 40, 48, 64, 128, 256})
}

func itoa(i int) string { return strconv.Itoa(i) }
