package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"sync"

	"github.com/jmarceno/duck-mem/assets"
)

// traySizes are the pixmap sizes handed to the StatusNotifierItem host, which
// picks the closest match for its own tray icon size.
var traySizes = []int{22, 24, 36, 48, 64, 128}

// The status badge the host layers over the logo.
var (
	busyColor    = color.NRGBA{R: 245, G: 174, B: 62, A: 255}
	stoppedColor = color.NRGBA{R: 128, G: 128, B: 128, A: 255}
	failedColor  = color.NRGBA{R: 220, G: 50, B: 50, A: 255}
)

// trayIcons holds the logo and the status badges pre-rendered at every tray
// size, so the ten-second refresh only swaps pixmap lists.
type trayIcons struct {
	logo    []iconPixmap
	busy    []iconPixmap
	stopped []iconPixmap
	failed  []iconPixmap
}

func newTrayIcons() (trayIcons, error) {
	src, err := logoImage()
	if err != nil {
		return trayIcons{}, err
	}
	var set trayIcons
	for _, size := range traySizes {
		logo := resampleArea(src, size)
		set.logo = append(set.logo, pixmap(logo))
		set.busy = append(set.busy, pixmap(statusBadge(size, busyColor)))
		set.failed = append(set.failed, pixmap(failureIcon(logo)))
		set.stopped = append(set.stopped, pixmap(statusBadge(size, stoppedColor)))
	}
	return set, nil
}

// icon includes the failure badge in the main pixmap so hosts that ignore
// OverlayIconPixmap still display unread errors.
func (i trayIcons) icon(s traySnapshot) []iconPixmap {
	if s.failure {
		return i.failed
	}
	return i.logo
}

// Failure takes precedence over the busy and stopped overlays.
func (i trayIcons) overlay(s traySnapshot) []iconPixmap {
	switch {
	case s.failure:
		return []iconPixmap{}
	case s.busy:
		return i.busy
	case !s.running:
		return i.stopped
	default:
		return []iconPixmap{}
	}
}

func failureIcon(logo *image.NRGBA) *image.NRGBA {
	size := logo.Bounds().Dx()
	img := image.NewNRGBA(logo.Bounds())
	draw.Draw(img, img.Bounds(), logo, logo.Bounds().Min, draw.Src)
	badgeSize := size / 2
	badge := statusBadge(badgeSize, failedColor)
	draw.Draw(img, image.Rect(size-badgeSize, 0, size, badgeSize), badge, image.Point{}, draw.Over)
	return img
}

var (
	logoOnce    sync.Once
	logoDecoded *image.NRGBA
	logoErr     error
)

func logoImage() (*image.NRGBA, error) {
	logoOnce.Do(func() {
		img, err := png.Decode(bytes.NewReader(assets.Logo))
		if err != nil {
			logoErr = err
			return
		}
		// The logo is 1254px square RGBA; tray sizes come straight off it.
		b := img.Bounds()
		logoDecoded = image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(logoDecoded, logoDecoded.Bounds(), img, b.Min, draw.Src)
	})
	return logoDecoded, logoErr
}

// resampleArea box-filters src down to size×size. It averages premultiplied
// alpha, so the logo's black outline does not bleed into its transparent
// margin and 55% of the source is not turned into a grey haze at 22px.
func resampleArea(src *image.NRGBA, size int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	sw, sh := float64(src.Bounds().Dx()), float64(src.Bounds().Dy())
	scaleX, scaleY := sw/float64(size), sh/float64(size)
	for dy := 0; dy < size; dy++ {
		y0, y1 := float64(dy)*scaleY, float64(dy+1)*scaleY
		for dx := 0; dx < size; dx++ {
			x0, x1 := float64(dx)*scaleX, float64(dx+1)*scaleX
			var r, g, b, a, weight float64
			for sy := int(math.Floor(y0)); sy < int(math.Ceil(y1)); sy++ {
				wy := overlap(float64(sy), float64(sy+1), y0, y1)
				if wy == 0 {
					continue
				}
				for sx := int(math.Floor(x0)); sx < int(math.Ceil(x1)); sx++ {
					wt := overlap(float64(sx), float64(sx+1), x0, x1) * wy
					if wt == 0 {
						continue
					}
					c := src.NRGBAAt(sx, sy)
					ca := float64(c.A) / 255
					weight += wt
					a += wt * ca
					r += wt * ca * float64(c.R) / 255
					g += wt * ca * float64(c.G) / 255
					b += wt * ca * float64(c.B) / 255
				}
			}
			o := dst.PixOffset(dx, dy)
			dst.Pix[o+3] = byte(clamp255(a / weight * 255))
			if a > 0 {
				dst.Pix[o+0] = byte(clamp255(r / a * 255))
				dst.Pix[o+1] = byte(clamp255(g / a * 255))
				dst.Pix[o+2] = byte(clamp255(b / a * 255))
			}
		}
	}
	return dst
}

// statusBadge draws the dot the host composites over the logo, centred in its
// canvas and left well inside it: hosts place the overlay in a corner, usually
// scaled to half the icon, so a full-canvas dot would land as a blob.
func statusBadge(size int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	center, radius := float64(size)/2, float64(size)*0.3
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)+0.5-center, float64(y)+0.5-center)
			// Antialias the rim over one pixel; a hard edge is ragged at 22px.
			cover := clamp01(radius + 0.5 - d)
			if cover == 0 {
				continue
			}
			o := img.PixOffset(x, y)
			img.Pix[o+0] = byte(clamp255(float64(c.R) * cover))
			img.Pix[o+1] = byte(clamp255(float64(c.G) * cover))
			img.Pix[o+2] = byte(clamp255(float64(c.B) * cover))
			img.Pix[o+3] = byte(clamp255(float64(c.A) * cover))
		}
	}
	return img
}

// pixmap packs an image the way StatusNotifierItem wants it: ARGB32 in
// network byte order.
func pixmap(src *image.NRGBA) iconPixmap {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	data := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		c := src.Pix[i*4 : i*4+4 : i*4+4]
		data[i*4+0], data[i*4+1], data[i*4+2], data[i*4+3] = c[3], c[0], c[1], c[2]
	}
	return iconPixmap{int32(w), int32(h), data}
}

// pickPixmap returns the rendered size closest to the host's tray size.
func pickPixmap(list []iconPixmap, size int) iconPixmap {
	if len(list) == 0 {
		return iconPixmap{int32(size), int32(size), make([]byte, size*size*4)}
	}
	best, dist := 0, int(list[0].Width)-size
	if dist < 0 {
		dist = -dist
	}
	for i, p := range list[1:] {
		d := int(p.Width) - size
		if d < 0 {
			d = -d
		}
		if d < dist {
			best, dist = i+1, d
		}
	}
	return list[best]
}

func overlap(lo, hi, a, b float64) float64 {
	if lo < a {
		lo = a
	}
	if hi > b {
		hi = b
	}
	if hi <= lo {
		return 0
	}
	return hi - lo
}

func clamp01(v float64) float64 {
	switch {
	case v <= 0:
		return 0
	case v >= 1:
		return 1
	default:
		return v
	}
}

func clamp255(v float64) int {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return int(v + 0.5)
	}
}
