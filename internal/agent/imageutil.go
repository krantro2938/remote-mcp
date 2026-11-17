package agent

import (
	"image"
	"image/draw"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

// cropRGBA copies r out of img into a new image whose origin is (0,0).
func cropRGBA(img *image.RGBA, r image.Rectangle) *image.RGBA {
	r = r.Intersect(img.Bounds())
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), img, r.Min, draw.Src)
	return out
}

func intersect(a, b proto.Rect) proto.Rect {
	r := image.Rect(a.X, a.Y, a.X+a.W, a.Y+a.H).Intersect(image.Rect(b.X, b.Y, b.X+b.W, b.Y+b.H))
	return proto.Rect{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}
}

// cropAbs crops an image positioned at abs (screen coordinates) to the
// screen-coordinate region. Returns the new image and its absolute rect.
func cropAbs(img *image.RGBA, abs, region proto.Rect) (*image.RGBA, proto.Rect) {
	in := intersect(abs, region)
	if in.W <= 0 || in.H <= 0 {
		return img, abs
	}
	local := image.Rect(in.X-abs.X, in.Y-abs.Y, in.X-abs.X+in.W, in.Y-abs.Y+in.H)
	return cropRGBA(img, local), in
}
