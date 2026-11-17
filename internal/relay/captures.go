package relay

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

// Capture is a stored screenshot. Every image shown to the model is a capture
// (zooms create derived captures), so coordinates can always be mapped back
// to screen pixels: screen = Origin + view_coord * Width / ViewW.
type Capture struct {
	ID       string      `json:"id"`
	Machine  string      `json:"machine"`
	Label    string      `json:"label,omitempty"`
	Created  time.Time   `json:"created"`
	Origin   proto.Point `json:"origin"`
	Width    int         `json:"width"`  // full resolution
	Height   int         `json:"height"` // full resolution
	ViewW    int         `json:"view_w"` // size of the image the model saw
	ViewH    int         `json:"view_h"`
	Source   string      `json:"source"`
	Title    string      `json:"title,omitempty"`
	ParentID string      `json:"parent_id,omitempty"`
	Bytes    int         `json:"bytes"`
}

// ToScreen maps a point in the image the model saw to absolute screen pixels.
func (c *Capture) ToScreen(x, y int) (int, int) {
	sx := c.Origin.X + int(float64(x)*float64(c.Width)/float64(max(1, c.ViewW))+0.5)
	sy := c.Origin.Y + int(float64(y)*float64(c.Height)/float64(max(1, c.ViewH))+0.5)
	return sx, sy
}

// Scale is full-resolution pixels per view pixel.
func (c *Capture) Scale() float64 { return float64(c.Width) / float64(max(1, c.ViewW)) }

type CaptureStore struct {
	dir string
	log *slog.Logger

	mu     sync.Mutex
	byID   map[string]*Capture
	last   map[string]string                 // machine -> last capture shown
	params map[string]proto.ScreenshotParams // machine -> last screenshot request
}

func NewCaptureStore(dir string, log *slog.Logger) (*CaptureStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	s := &CaptureStore{dir: dir, log: log, byID: map[string]*Capture{}, last: map[string]string{}, params: map[string]proto.ScreenshotParams{}}
	metas, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, m := range metas {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var c Capture
		if json.Unmarshal(data, &c) == nil && c.ID != "" {
			s.byID[c.ID] = &c
		}
	}
	return s, nil
}

var idRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

func newCaptureID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *CaptureStore) pngPath(id string) string  { return filepath.Join(s.dir, id+".png") }
func (s *CaptureStore) metaPath(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *CaptureStore) saveMeta(c *Capture) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(s.metaPath(c.ID), data, 0o640)
}

// Add stores a full-resolution PNG and renders the view the model will see.
func (s *CaptureStore) Add(c *Capture, pngData []byte, maxSize int) (view []byte, mime string, err error) {
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, "", fmt.Errorf("decode screenshot: %w", err)
	}
	c.ID = newCaptureID()
	c.Created = time.Now().UTC()
	c.Width, c.Height = img.Bounds().Dx(), img.Bounds().Dy()
	c.Bytes = len(pngData)
	view, mime, c.ViewW, c.ViewH, err = renderView(img, maxSize)
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(s.pngPath(c.ID), pngData, 0o640); err != nil {
		return nil, "", err
	}
	if err := s.saveMeta(c); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	s.byID[c.ID] = c
	s.last[c.Machine] = c.ID
	s.mu.Unlock()
	return view, mime, nil
}

// renderView downscales img so its longest side is at most maxSize and its
// area at most ~1.2 MP, which is what vision models read best, and encodes it.
func renderView(img image.Image, maxSize int) ([]byte, string, int, int, error) {
	if maxSize <= 0 {
		maxSize = 1400
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	scale := 1.0
	if long := max(w, h); long > maxSize {
		scale = float64(maxSize) / float64(long)
	}
	const maxArea = 1_200_000.0
	if a := float64(w) * float64(h) * scale * scale; a > maxArea && maxSize <= 2000 {
		scale *= math.Sqrt(maxArea / a)
	}
	vw, vh := max(1, int(float64(w)*scale+0.5)), max(1, int(float64(h)*scale+0.5))
	var src image.Image = img
	if vw != w || vh != h {
		dst := image.NewRGBA(image.Rect(0, 0, vw, vh))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		src = dst
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, src); err != nil {
		return nil, "", 0, 0, err
	}
	if buf.Len() <= 1_500_000 {
		return buf.Bytes(), "image/png", vw, vh, nil
	}
	buf.Reset()
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 88}); err != nil {
		return nil, "", 0, 0, err
	}
	return buf.Bytes(), "image/jpeg", vw, vh, nil
}

func (s *CaptureStore) Get(id string) (*Capture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byID[id]
	if c == nil {
		return nil, fmt.Errorf("unknown capture %q", id)
	}
	return c, nil
}

// Latest returns the last image shown for a machine (nil if none).
func (s *CaptureStore) Latest(machine string) *Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[s.last[machine]]
}

func (s *CaptureStore) SetLastParams(machine string, p proto.ScreenshotParams) {
	s.mu.Lock()
	s.params[machine] = p
	s.mu.Unlock()
}

func (s *CaptureStore) LastParams(machine string) proto.ScreenshotParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params[machine]
}

func (s *CaptureStore) ReadPNG(id string) ([]byte, error) {
	if !idRe.MatchString(id) {
		return nil, errors.New("bad id")
	}
	return os.ReadFile(s.pngPath(id))
}

// Zoom crops a region (in the parent's view coordinates) from the parent's
// full-resolution image and stores it as a new capture.
func (s *CaptureStore) Zoom(parent *Capture, x, y, w, h, maxSize int, label string) (*Capture, []byte, string, error) {
	data, err := s.ReadPNG(parent.ID)
	if err != nil {
		return nil, nil, "", err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, "", err
	}
	if w <= 0 || h <= 0 {
		x, y, w, h = 0, 0, parent.ViewW, parent.ViewH
	}
	sc := parent.Scale()
	r := image.Rect(int(float64(x)*sc), int(float64(y)*sc), int(float64(x+w)*sc+0.5), int(float64(y+h)*sc+0.5)).Intersect(img.Bounds())
	if r.Empty() {
		return nil, nil, "", errors.New("zoom region is outside the image")
	}
	crop := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(crop, crop.Bounds(), img, r.Min, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, crop); err != nil {
		return nil, nil, "", err
	}
	c := &Capture{
		Machine: parent.Machine, Label: label, Source: parent.Source + "+zoom", Title: parent.Title, ParentID: parent.ID,
		Origin: proto.Point{X: parent.Origin.X + r.Min.X, Y: parent.Origin.Y + r.Min.Y},
	}
	view, mime, err := s.Add(c, buf.Bytes(), maxSize)
	return c, view, mime, err
}

func (s *CaptureStore) SetLabel(id, label string) (*Capture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byID[id]
	if c == nil {
		return nil, fmt.Errorf("unknown capture %q", id)
	}
	c.Label = label
	return c, s.saveMeta(c)
}

func (s *CaptureStore) List(machine string, labeledOnly bool) []*Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Capture
	for _, c := range s.byID {
		if machine != "" && !strings.EqualFold(c.Machine, machine) {
			continue
		}
		if labeledOnly && c.Label == "" {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Cleanup deletes unlabeled captures older than ttl and labeled ones older than labeledTTL.
func (s *CaptureStore) Cleanup(ttl, labeledTTL time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, c := range s.byID {
		limit := ttl
		if c.Label != "" {
			limit = labeledTTL
		}
		if time.Since(c.Created) > limit {
			os.Remove(s.pngPath(id))
			os.Remove(s.metaPath(id))
			delete(s.byID, id)
			n++
		}
	}
	return n
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

// WriteZip writes the given captures as <label or id>.png into a zip archive.
func (s *CaptureStore) WriteZip(w io.Writer, caps []*Capture) error {
	zw := zip.NewWriter(w)
	used := map[string]int{}
	// Oldest first so numbering follows the order they were taken.
	sort.Slice(caps, func(i, j int) bool { return caps[i].Created.Before(caps[j].Created) })
	for _, c := range caps {
		name := c.ID
		if c.Label != "" {
			name = strings.Trim(unsafeName.ReplaceAllString(c.Label, "_"), "_")
		}
		if used[name]++; used[name] > 1 {
			name = fmt.Sprintf("%s_%d", name, used[name])
		}
		data, err := s.ReadPNG(c.ID)
		if err != nil {
			continue
		}
		f, err := zw.Create(name + ".png")
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
	}
	return zw.Close()
}
