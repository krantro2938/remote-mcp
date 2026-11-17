package agent

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

// DemoBackend is a fake desktop used for testing the relay end to end on
// machines that are not Windows. It draws a synthetic screen with two
// "windows", marks every click with a dot, and records every input action.
type DemoBackend struct {
	mu     sync.Mutex
	cursor proto.Point
	clicks []proto.Point
	events []string
	wins   []proto.Window
	fg     uint64
}

func NewDemoBackend() *DemoBackend {
	return &DemoBackend{
		wins: []proto.Window{
			{Handle: 101, Title: "Vivado 2023.2 - uart_ctrl - [C:/labs/uart/uart.xpr]", PID: 4242, Process: "vivado.exe", Rect: proto.Rect{X: 100, Y: 80, W: 1400, H: 850}},
			{Handle: 102, Title: "Notepad", PID: 777, Process: "notepad.exe", Rect: proto.Rect{X: 1300, Y: 600, W: 500, H: 400}},
		},
		fg: 101,
	}
}

func (d *DemoBackend) Name() string { return "demo" }

func (d *DemoBackend) Events() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func (d *DemoBackend) Displays() (proto.Rect, []proto.Monitor, error) {
	scr := proto.Rect{W: 1920, H: 1080}
	return scr, []proto.Monitor{{Index: 1, Bounds: scr, Primary: true}}, nil
}

func (d *DemoBackend) render() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{30, 80, 140, 255}}, image.Point{}, draw.Src)
	colors := map[uint64]color.RGBA{101: {235, 235, 235, 255}, 102: {255, 255, 210, 255}}
	for i := len(d.wins) - 1; i >= 0; i-- {
		w := d.wins[i]
		if w.Minimized {
			continue
		}
		r := image.Rect(w.Rect.X, w.Rect.Y, w.Rect.X+w.Rect.W, w.Rect.Y+w.Rect.H)
		draw.Draw(img, r, &image.Uniform{colors[w.Handle]}, image.Point{}, draw.Src)
		bar := image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+30)
		draw.Draw(img, bar, &image.Uniform{color.RGBA{60, 60, 70, 255}}, image.Point{}, draw.Src)
	}
	for _, c := range d.clicks {
		draw.Draw(img, image.Rect(c.X-4, c.Y-4, c.X+5, c.Y+5), &image.Uniform{color.RGBA{220, 30, 30, 255}}, image.Point{}, draw.Src)
	}
	return img
}

func (d *DemoBackend) find(title string, handle uint64) (*proto.Window, error) {
	for i := range d.wins {
		w := &d.wins[i]
		if (handle != 0 && w.Handle == handle) || (title != "" && strings.Contains(strings.ToLower(w.Title), strings.ToLower(title))) {
			return w, nil
		}
	}
	return nil, fmt.Errorf("no visible window matches %q", title)
}

func (d *DemoBackend) Capture(p proto.ScreenshotParams) (*image.RGBA, proto.Point, string, string, uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	full := d.render()
	abs := proto.Rect{W: 1920, H: 1080}
	source, title, handle := "screen", "", uint64(0)
	if p.Window != "" || p.Handle != 0 {
		w, err := d.find(p.Window, p.Handle)
		if err != nil {
			return nil, proto.Point{}, "", "", 0, err
		}
		abs, source, title, handle = w.Rect, "window:printwindow", w.Title, w.Handle
	}
	if p.Region != nil {
		abs = intersect(abs, *p.Region)
		source += "+region"
	}
	img := cropRGBA(full, image.Rect(abs.X, abs.Y, abs.X+abs.W, abs.Y+abs.H))
	return img, proto.Point{X: abs.X, Y: abs.Y}, source, title, handle, nil
}

func (d *DemoBackend) Input(p proto.InputParams) (proto.Point, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p.HasXY {
		d.cursor = proto.Point{X: p.X, Y: p.Y}
	}
	switch p.Action {
	case "click":
		d.clicks = append(d.clicks, d.cursor)
		d.events = append(d.events, fmt.Sprintf("click %s %d,%d x%d", p.Button, d.cursor.X, d.cursor.Y, max(1, p.Clicks)))
	case "drag":
		d.events = append(d.events, fmt.Sprintf("drag %d,%d -> %d,%d", p.X, p.Y, p.X2, p.Y2))
		d.cursor = proto.Point{X: p.X2, Y: p.Y2}
	case "type":
		d.events = append(d.events, "type "+p.Text)
	case "key":
		d.events = append(d.events, "key "+p.Keys)
	case "scroll":
		d.events = append(d.events, fmt.Sprintf("scroll %d at %d,%d", p.Amount, d.cursor.X, d.cursor.Y))
	case "move", "down", "up":
		d.events = append(d.events, fmt.Sprintf("%s %d,%d", p.Action, d.cursor.X, d.cursor.Y))
	default:
		return d.cursor, fmt.Errorf("unknown input action %q", p.Action)
	}
	return d.cursor, nil
}

func (d *DemoBackend) Windows() ([]proto.Window, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]proto.Window(nil), d.wins...)
	for i := range out {
		out[i].Foreground = out[i].Handle == d.fg
	}
	return out, nil
}

func (d *DemoBackend) Window(p proto.WindowParams) (*proto.Window, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w, err := d.find(p.Window, p.Handle)
	if err != nil {
		return nil, err
	}
	switch p.Action {
	case "", "focus", "restore":
		w.Minimized = false
	case "maximize":
		w.Minimized, w.Maximized, w.Rect = false, true, proto.Rect{W: 1920, H: 1040}
	case "minimize":
		w.Minimized = true
	case "move":
		if p.Rect != nil {
			w.Rect = *p.Rect
		}
	}
	d.fg = w.Handle
	d.events = append(d.events, "window "+p.Action+" "+w.Title)
	out := *w
	out.Foreground = true
	return &out, nil
}
