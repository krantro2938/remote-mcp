//go:build windows

package agent

import (
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

// Compile-time checks that the INPUT structs match the 64-bit Win32 layout.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(mouseINPUT{})-40]
	_ = [1]struct{}{}[unsafe.Sizeof(keybdINPUT{})-40]
)

type winBackend struct{}

// NewPlatformBackend returns the Windows backend. It makes the process
// per-monitor DPI aware so that screenshots are full resolution and screen
// coordinates are physical pixels on scaled (125%/150%) displays.
func NewPlatformBackend() Backend {
	if pSetProcessDpiAwarenessContext.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == (HANDLE)-4
		if !boolCall(pSetProcessDpiAwarenessContext, ^uintptr(3)) {
			boolCall(pSetProcessDPIAware)
		}
	} else {
		boolCall(pSetProcessDPIAware)
	}
	return &winBackend{}
}

func (b *winBackend) Name() string { return "windows" }

// ---------- enumeration helpers (callbacks are created once; NewCallback leaks) ----------

var (
	enumMu      sync.Mutex
	enumHandles []uintptr
	enumMons    []proto.Monitor

	enumWindowsCB = syscall.NewCallback(func(h, _ uintptr) uintptr {
		enumHandles = append(enumHandles, h)
		return 1
	})
	enumMonitorsCB = syscall.NewCallback(func(hmon, _, _, _ uintptr) uintptr {
		var mi monitorInfo
		mi.Size = uint32(unsafe.Sizeof(mi))
		if boolCall(pGetMonitorInfoW, hmon, uintptr(unsafe.Pointer(&mi))) {
			enumMons = append(enumMons, proto.Monitor{
				Index:   len(enumMons) + 1,
				Bounds:  toRect(mi.Monitor),
				Primary: mi.Flags&monitorInfoPrimary != 0,
			})
		}
		return 1
	})
)

func toRect(r winRect) proto.Rect {
	return proto.Rect{X: int(r.Left), Y: int(r.Top), W: int(r.Right - r.Left), H: int(r.Bottom - r.Top)}
}

func topLevelWindows() []uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumHandles = enumHandles[:0]
	pEnumWindows.Call(enumWindowsCB, 0)
	return append([]uintptr(nil), enumHandles...)
}

func (b *winBackend) Displays() (proto.Rect, []proto.Monitor, error) {
	screen := proto.Rect{
		X: getSystemMetrics(smXVirtualScreen), Y: getSystemMetrics(smYVirtualScreen),
		W: getSystemMetrics(smCXVirtualScreen), H: getSystemMetrics(smCYVirtualScreen),
	}
	enumMu.Lock()
	enumMons = enumMons[:0]
	pEnumDisplayMonitors.Call(0, 0, enumMonitorsCB, 0)
	mons := append([]proto.Monitor(nil), enumMons...)
	enumMu.Unlock()
	if screen.W == 0 || screen.H == 0 {
		return screen, mons, errors.New("could not read screen size")
	}
	return screen, mons, nil
}

// ---------- windows ----------

func describeWindow(h uintptr, fg uintptr) proto.Window {
	fb, _ := frameBounds(h)
	pid := windowPID(h)
	return proto.Window{
		Handle:     uint64(h),
		Title:      windowText(h),
		PID:        pid,
		Process:    processName(pid),
		Rect:       toRect(fb),
		Minimized:  boolCall(pIsIconic, h),
		Maximized:  boolCall(pIsZoomed, h),
		Foreground: h == fg,
	}
}

func (b *winBackend) Windows() ([]proto.Window, error) {
	fg, _, _ := pGetForegroundWindow.Call()
	var out []proto.Window
	for _, h := range topLevelWindows() {
		if !boolCall(pIsWindowVisible, h) || isCloaked(h) {
			continue
		}
		if windowText(h) == "" {
			continue
		}
		w := describeWindow(h, fg)
		if (w.Rect.W <= 1 || w.Rect.H <= 1) && !w.Minimized {
			continue
		}
		// Skip the desktop shell itself.
		if w.Title == "Program Manager" && strings.EqualFold(w.Process, "explorer.exe") {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

func (b *winBackend) findWindow(title string, handle uint64) (uintptr, error) {
	if handle != 0 {
		h := uintptr(handle)
		if !boolCall(pIsWindow, h) {
			return 0, fmt.Errorf("window handle %d no longer exists", handle)
		}
		return h, nil
	}
	ws, err := b.Windows()
	if err != nil {
		return 0, err
	}
	needle := strings.ToLower(strings.TrimSpace(title))
	if needle == "" {
		return 0, errors.New("no window title or handle given")
	}
	for _, w := range ws { // EnumWindows returns top-most first
		if strings.Contains(strings.ToLower(w.Title), needle) {
			return uintptr(w.Handle), nil
		}
	}
	for _, w := range ws {
		if strings.EqualFold(w.Process, needle) || strings.EqualFold(strings.TrimSuffix(w.Process, ".exe"), needle) {
			return uintptr(w.Handle), nil
		}
	}
	titles := make([]string, 0, len(ws))
	for i, w := range ws {
		if i >= 20 {
			break
		}
		titles = append(titles, fmt.Sprintf("%q", w.Title))
	}
	return 0, fmt.Errorf("no visible window matches %q; open windows: %s", title, strings.Join(titles, ", "))
}

func (b *winBackend) focus(h uintptr) {
	if boolCall(pIsIconic, h) {
		pShowWindow.Call(h, swRestore)
		time.Sleep(250 * time.Millisecond)
	}
	if fg, _, _ := pGetForegroundWindow.Call(); fg == h {
		return
	}
	pSetForegroundWindow.Call(h)
	if fg, _, _ := pGetForegroundWindow.Call(); fg != h {
		// Windows refuses SetForegroundWindow from background processes unless
		// a key was "recently pressed"; tapping Alt satisfies that rule.
		_ = tapKey(0x12)
		pSetForegroundWindow.Call(h)
	}
	pBringWindowToTop.Call(h)
	time.Sleep(150 * time.Millisecond)
}

func (b *winBackend) Window(p proto.WindowParams) (*proto.Window, error) {
	h, err := b.findWindow(p.Window, p.Handle)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(p.Action) {
	case "", "focus":
		b.focus(h)
	case "maximize":
		pShowWindow.Call(h, swMaximize)
		b.focus(h)
	case "minimize":
		pShowWindow.Call(h, swMinimize)
	case "restore":
		pShowWindow.Call(h, swRestore)
		b.focus(h)
	case "move":
		if p.Rect == nil || p.Rect.W <= 0 || p.Rect.H <= 0 {
			return nil, errors.New("move needs rect {x,y,w,h}")
		}
		if boolCall(pIsZoomed, h) || boolCall(pIsIconic, h) {
			pShowWindow.Call(h, swRestore)
			time.Sleep(200 * time.Millisecond)
		}
		pMoveWindow.Call(h, uintptr(p.Rect.X), uintptr(p.Rect.Y), uintptr(p.Rect.W), uintptr(p.Rect.H), 1)
		b.focus(h)
	case "close":
		pPostMessageW.Call(h, wmClose, 0, 0)
		time.Sleep(300 * time.Millisecond)
		if !boolCall(pIsWindow, h) {
			return &proto.Window{Handle: uint64(h), Title: "(closed)"}, nil
		}
	default:
		return nil, fmt.Errorf("unknown window action %q", p.Action)
	}
	time.Sleep(200 * time.Millisecond)
	fg, _, _ := pGetForegroundWindow.Call()
	w := describeWindow(h, fg)
	return &w, nil
}

// ---------- capture ----------

func dibToRGBA(dc, bmp uintptr, w, h int) (*image.RGBA, error) {
	bi := bitmapInfo{Header: bitmapInfoHeader{
		Width: int32(w), Height: -int32(h), // negative height = top-down rows
		Planes: 1, BitCount: 32,
	}}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	lines, _, _ := pGetDIBits.Call(dc, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&img.Pix[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if int(lines) != h {
		return nil, errors.New("GetDIBits failed")
	}
	// BGRA -> RGBA, opaque.
	px := img.Pix
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+2] = px[i+2], px[i]
		px[i+3] = 255
	}
	return img, nil
}

func captureScreen(r proto.Rect) (*image.RGBA, error) {
	if r.W <= 0 || r.H <= 0 {
		return nil, fmt.Errorf("empty capture area %+v", r)
	}
	hdc, _, _ := pGetDC.Call(0)
	if hdc == 0 {
		return nil, errors.New("GetDC failed (is the session locked or disconnected?)")
	}
	defer pReleaseDC.Call(0, hdc)
	mem, _, _ := pCreateCompatibleDC.Call(hdc)
	if mem == 0 {
		return nil, errors.New("CreateCompatibleDC failed")
	}
	defer pDeleteDC.Call(mem)
	bmp, _, _ := pCreateCompatibleBitmap.Call(hdc, uintptr(r.W), uintptr(r.H))
	if bmp == 0 {
		return nil, errors.New("CreateCompatibleBitmap failed")
	}
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(mem, bmp)
	ok := boolCall(pBitBlt, mem, 0, 0, uintptr(r.W), uintptr(r.H), hdc, uintptr(r.X), uintptr(r.Y), srcCopy|captureBlt)
	pSelectObject.Call(mem, old) // GetDIBits needs the bitmap deselected
	if !ok {
		return nil, errors.New("BitBlt failed (is the session locked or disconnected?)")
	}
	return dibToRGBA(mem, bmp, r.W, r.H)
}

// printWindow renders a window via PrintWindow, which works even when the
// window is partly covered. Returns the image cropped to the visible frame.
func printWindow(h uintptr) (*image.RGBA, proto.Rect, error) {
	wr, ok := windowRect(h)
	if !ok {
		return nil, proto.Rect{}, errors.New("GetWindowRect failed")
	}
	w, ht := int(wr.Right-wr.Left), int(wr.Bottom-wr.Top)
	if w <= 0 || ht <= 0 {
		return nil, proto.Rect{}, errors.New("window has no size")
	}
	hdc, _, _ := pGetDC.Call(0)
	if hdc == 0 {
		return nil, proto.Rect{}, errors.New("GetDC failed")
	}
	defer pReleaseDC.Call(0, hdc)
	mem, _, _ := pCreateCompatibleDC.Call(hdc)
	defer pDeleteDC.Call(mem)
	bmp, _, _ := pCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(ht))
	if bmp == 0 {
		return nil, proto.Rect{}, errors.New("CreateCompatibleBitmap failed")
	}
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(mem, bmp)
	ok = boolCall(pPrintWindow, h, mem, pwRenderFullContent)
	pSelectObject.Call(mem, old)
	if !ok {
		return nil, proto.Rect{}, errors.New("PrintWindow failed")
	}
	img, err := dibToRGBA(mem, bmp, w, ht)
	if err != nil {
		return nil, proto.Rect{}, err
	}
	fb, _ := frameBounds(h)
	crop := image.Rect(int(fb.Left-wr.Left), int(fb.Top-wr.Top), int(fb.Right-wr.Left), int(fb.Bottom-wr.Top)).Intersect(img.Bounds())
	if crop.Empty() {
		crop = img.Bounds()
	}
	abs := proto.Rect{X: int(wr.Left) + crop.Min.X, Y: int(wr.Top) + crop.Min.Y, W: crop.Dx(), H: crop.Dy()}
	return cropRGBA(img, crop), abs, nil
}

func (b *winBackend) Capture(p proto.ScreenshotParams) (*image.RGBA, proto.Point, string, string, uint64, error) {
	if p.Window != "" || p.Handle != 0 {
		h, err := b.findWindow(p.Window, p.Handle)
		if err != nil {
			return nil, proto.Point{}, "", "", 0, err
		}
		title := windowText(h)
		if boolCall(pIsIconic, h) {
			pShowWindow.Call(h, swRestore)
			time.Sleep(400 * time.Millisecond)
		}
		img, abs, err := printWindow(h)
		source := "window:printwindow"
		if err != nil || isBlack(img) {
			// Some apps (GPU-rendered) don't support PrintWindow: bring the
			// window to the front and grab that part of the screen instead.
			b.focus(h)
			time.Sleep(250 * time.Millisecond)
			fb, _ := frameBounds(h)
			abs = toRect(fb)
			img, err = captureScreen(abs)
			source = "window:screen"
			if err != nil {
				return nil, proto.Point{}, "", "", 0, err
			}
		}
		if p.Region != nil {
			img, abs = cropAbs(img, abs, *p.Region)
			source += "+region"
		}
		return img, proto.Point{X: abs.X, Y: abs.Y}, source, title, uint64(h), nil
	}

	screen, mons, err := b.Displays()
	if err != nil {
		return nil, proto.Point{}, "", "", 0, err
	}
	area, source := screen, "screen"
	if p.Monitor > 0 {
		if p.Monitor > len(mons) {
			return nil, proto.Point{}, "", "", 0, fmt.Errorf("monitor %d does not exist (have %d)", p.Monitor, len(mons))
		}
		area, source = mons[p.Monitor-1].Bounds, fmt.Sprintf("monitor:%d", p.Monitor)
	}
	if p.Region != nil {
		area, source = intersect(area, *p.Region), "region"
	}
	img, err := captureScreen(area)
	if err != nil {
		return nil, proto.Point{}, "", "", 0, err
	}
	return img, proto.Point{X: area.X, Y: area.Y}, source, "", 0, nil
}

// ---------- input ----------

func cursorPos() proto.Point {
	var p winPoint
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return proto.Point{X: int(p.X), Y: int(p.Y)}
}

func setCursor(x, y int) error {
	if !boolCall(pSetCursorPos, uintptr(x), uintptr(y)) {
		return fmt.Errorf("SetCursorPos(%d,%d) failed", x, y)
	}
	return nil
}

func mouseEvent(flags uint32, data int32) error {
	in := mouseINPUT{Type: inputMouse, Mi: mouseInput{Flags: flags, MouseData: uint32(data)}}
	if err := sendInputs(unsafe.Pointer(&in), 1, unsafe.Sizeof(in)); err != nil {
		return fmt.Errorf("SendInput failed (a UAC prompt or elevated window may be blocking input): %v", err)
	}
	return nil
}

func buttonFlags(button string) (down, up uint32, err error) {
	switch strings.ToLower(button) {
	case "", "left":
		return mouseLeftDown, mouseLeftUp, nil
	case "right":
		return mouseRightDown, mouseRightUp, nil
	case "middle":
		return mouseMiddleDown, mouseMiddleUp, nil
	}
	return 0, 0, fmt.Errorf("unknown button %q", button)
}

func (b *winBackend) Input(p proto.InputParams) (proto.Point, error) {
	moveTo := func() error {
		if !p.HasXY {
			return nil
		}
		if err := setCursor(p.X, p.Y); err != nil {
			return err
		}
		time.Sleep(40 * time.Millisecond)
		return nil
	}
	var err error
	switch strings.ToLower(p.Action) {
	case "move":
		err = moveTo()
	case "click":
		down, up, e := buttonFlags(p.Button)
		if e != nil {
			return cursorPos(), e
		}
		if err = moveTo(); err != nil {
			break
		}
		for i := 0; i < max(1, p.Clicks); i++ {
			if err = mouseEvent(down, 0); err != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
			if err = mouseEvent(up, 0); err != nil {
				break
			}
			time.Sleep(60 * time.Millisecond)
		}
	case "down", "up":
		down, up, e := buttonFlags(p.Button)
		if e != nil {
			return cursorPos(), e
		}
		if err = moveTo(); err != nil {
			break
		}
		if p.Action == "down" {
			err = mouseEvent(down, 0)
		} else {
			err = mouseEvent(up, 0)
		}
	case "drag":
		down, up, e := buttonFlags(p.Button)
		if e != nil {
			return cursorPos(), e
		}
		if err = moveTo(); err != nil {
			break
		}
		if err = mouseEvent(down, 0); err != nil {
			break
		}
		const steps = 20
		for i := 1; i <= steps; i++ {
			x := p.X + (p.X2-p.X)*i/steps
			y := p.Y + (p.Y2-p.Y)*i/steps
			_ = setCursor(x, y)
			time.Sleep(15 * time.Millisecond)
		}
		time.Sleep(60 * time.Millisecond)
		err = mouseEvent(up, 0)
	case "scroll":
		if err = moveTo(); err != nil {
			break
		}
		amount := p.Amount
		if amount == 0 {
			amount = -3
		}
		flag := uint32(mouseWheel)
		if p.Horiz {
			flag = mouseHWheel
		}
		err = mouseEvent(flag, int32(amount*120))
	case "type":
		err = typeText(p.Text)
	case "key":
		for i := 0; i < max(1, p.Repeat); i++ {
			if err = pressCombo(p.Keys); err != nil {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
	default:
		err = fmt.Errorf("unknown input action %q", p.Action)
	}
	return cursorPos(), err
}

var namedKeys = map[string]uint16{
	"ctrl": 0x11, "control": 0x11, "shift": 0x10, "alt": 0x12, "menu": 0x12,
	"win": 0x5B, "windows": 0x5B, "meta": 0x5B, "cmd": 0x5B, "super": 0x5B,
	"enter": 0x0D, "return": 0x0D, "tab": 0x09, "esc": 0x1B, "escape": 0x1B,
	"space": 0x20, "backspace": 0x08, "delete": 0x2E, "del": 0x2E, "insert": 0x2D, "ins": 0x2D,
	"home": 0x24, "end": 0x23, "pageup": 0x21, "pgup": 0x21, "pagedown": 0x22, "pgdn": 0x22,
	"left": 0x25, "up": 0x26, "right": 0x27, "down": 0x28,
	"printscreen": 0x2C, "prtsc": 0x2C, "capslock": 0x14, "apps": 0x5D, "contextmenu": 0x5D,
	"plus": 0xBB, "minus": 0xBD, "comma": 0xBC, "period": 0xBE,
}

var extendedKeys = map[uint16]bool{
	0x2D: true, 0x2E: true, 0x24: true, 0x23: true, 0x21: true, 0x22: true,
	0x25: true, 0x26: true, 0x27: true, 0x28: true, 0x5B: true, 0x5D: true,
}

func isModifier(vk uint16) bool { return vk == 0x11 || vk == 0x10 || vk == 0x12 || vk == 0x5B }

// keyVK resolves a key name to a virtual-key code plus any modifiers the
// character itself needs (e.g. '?' needs shift on a US layout).
func keyVK(name string) (vk uint16, mods []uint16, err error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if v, ok := namedKeys[n]; ok {
		return v, nil, nil
	}
	if len(n) >= 2 && n[0] == 'f' {
		var f int
		if _, e := fmt.Sscanf(n, "f%d", &f); e == nil && f >= 1 && f <= 24 {
			return uint16(0x70 + f - 1), nil, nil
		}
	}
	r := []rune(name)
	if len(r) == 1 {
		c := unicode.ToUpper(r[0])
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return uint16(c), nil, nil
		}
		res, _, _ := pVkKeyScanW.Call(uintptr(r[0]))
		if int16(res) != -1 {
			vk = uint16(res & 0xFF)
			state := (res >> 8) & 0xFF
			if state&1 != 0 {
				mods = append(mods, 0x10)
			}
			if state&2 != 0 {
				mods = append(mods, 0x11)
			}
			if state&4 != 0 {
				mods = append(mods, 0x12)
			}
			return vk, mods, nil
		}
	}
	return 0, nil, fmt.Errorf("unknown key %q", name)
}

func keyInput(vk uint16, up bool) keybdINPUT {
	scan, _, _ := pMapVirtualKeyW.Call(uintptr(vk), 0)
	var flags uint32
	if up {
		flags |= keyUp
	}
	if extendedKeys[vk] {
		flags |= keyExtended
	}
	return keybdINPUT{Type: inputKeyboard, Ki: keybdInput{Vk: vk, Scan: uint16(scan), Flags: flags}}
}

func sendKeys(ins []keybdINPUT) error {
	if len(ins) == 0 {
		return nil
	}
	if err := sendInputs(unsafe.Pointer(&ins[0]), len(ins), unsafe.Sizeof(ins[0])); err != nil {
		return fmt.Errorf("SendInput failed (a UAC prompt or elevated window may be blocking input): %v", err)
	}
	return nil
}

func tapKey(vk uint16) error {
	return sendKeys([]keybdINPUT{keyInput(vk, false), keyInput(vk, true)})
}

func pressCombo(combo string) error {
	combo = strings.TrimSpace(combo)
	if combo == "" {
		return errors.New("no keys given")
	}
	var parts []string
	if combo == "+" {
		parts = []string{"+"}
	} else {
		parts = strings.Split(combo, "+")
		// "ctrl++" -> ["ctrl", "", ""]: the last key is a literal plus.
		if strings.HasSuffix(combo, "++") {
			parts = append(parts[:len(parts)-2], "+")
		}
	}
	var keys []uint16
	seen := map[uint16]bool{}
	for _, part := range parts {
		vk, mods, err := keyVK(part)
		if err != nil {
			return err
		}
		for _, m := range append(mods, vk) {
			if !seen[m] {
				seen[m] = true
				keys = append(keys, m)
			}
		}
	}
	// Modifiers first, then the rest; release in reverse order.
	var ordered []uint16
	for _, k := range keys {
		if isModifier(k) {
			ordered = append(ordered, k)
		}
	}
	for _, k := range keys {
		if !isModifier(k) {
			ordered = append(ordered, k)
		}
	}
	ins := make([]keybdINPUT, 0, len(ordered)*2)
	for _, k := range ordered {
		ins = append(ins, keyInput(k, false))
	}
	for i := len(ordered) - 1; i >= 0; i-- {
		ins = append(ins, keyInput(ordered[i], true))
	}
	return sendKeys(ins)
}

func typeText(text string) error {
	for _, r := range text {
		var err error
		switch r {
		case '\r':
			continue
		case '\n':
			err = tapKey(0x0D)
		case '\t':
			err = tapKey(0x09)
		default:
			units := utf16.Encode([]rune{r})
			ins := make([]keybdINPUT, 0, len(units)*2)
			for _, u := range units {
				ins = append(ins, keybdINPUT{Type: inputKeyboard, Ki: keybdInput{Scan: u, Flags: keyUnicode}})
			}
			for _, u := range units {
				ins = append(ins, keybdINPUT{Type: inputKeyboard, Ki: keybdInput{Scan: u, Flags: keyUnicode | keyUp}})
			}
			err = sendKeys(ins)
		}
		if err != nil {
			return err
		}
		// Java apps (Vivado) drop characters when input arrives too fast.
		time.Sleep(8 * time.Millisecond)
	}
	return nil
}
