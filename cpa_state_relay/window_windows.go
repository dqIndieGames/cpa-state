package main

import (
	"fmt"
	"image/color"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var iconBlue = color.NRGBA{110, 170, 230, 255}
var user32 = syscall.NewLazyDLL("user32.dll")
var gdi32 = syscall.NewLazyDLL("gdi32.dll")
var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var nativeWindow atomic.Uintptr
var uiRelay *relay
var uiScale = 1.0
var uiExpanded bool
var uiPage int
var uiRows = 1
var uiFooter bool
var uiIcon uintptr
var wndProcCallback = syscall.NewCallback(windowProc)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }
type winMsg struct {
	Hwnd           uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Pt             point
	Private        uint32
}
type paintStruct struct {
	DC                 uintptr
	Erase              int32
	Paint              rect
	Restore, IncUpdate int32
	Reserved           [32]byte
}
type wndClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background uintptr
	Menu, Name                         *uint16
	SmallIcon                          uintptr
}
type bitmapInfo struct {
	Size                   uint32
	Width, Height          int32
	Planes, BitCount       uint16
	Compression, SizeImage uint32
	XPels, YPels           int32
	Used, Important        uint32
}

//go:uintptrescapes
func ucall(name string, args ...uintptr) uintptr {
	x, _, _ := user32.NewProc(name).Call(args...)
	return x
}

//go:uintptrescapes
func gcall(name string, args ...uintptr) uintptr {
	x, _, _ := gdi32.NewProc(name).Call(args...)
	return x
}
func wide(s string) *uint16           { p, _ := syscall.UTF16PtrFromString(s); return p }
func scaled(x int) int32              { return int32(float64(x)*uiScale + 0.5) }
func currentPanelLayout() panelLayout { return makePanelLayout(uiExpanded, uiRows, uiFooter) }
func uiWidth() int32                  { return scaled(currentPanelLayout().Width) }
func uiHeight() int32                 { return scaled(currentPanelLayout().Height) }
func panelAccounts(s statusView) []accountView {
	if len(s.Accounts) == 0 {
		return []accountView{{State: s}}
	}
	if uiPage*3 >= len(s.Accounts) {
		uiPage = 0
	}
	start := uiPage * 3
	return s.Accounts[start:min(start+3, len(s.Accounts))]
}
func resizePanel(h uintptr, force ...bool) {
	s := uiRelay.status()
	rows := len(panelAccounts(s))
	footer := len(s.Accounts) > 3 || s.Error != ""
	if rows == uiRows && footer == uiFooter && !(len(force) > 0 && force[0]) {
		return
	}
	uiRows = rows
	uiFooter = footer
	rc := rect{}
	ucall("GetWindowRect", h, uintptr(unsafe.Pointer(&rc)))
	var monitor struct {
		Size         uint32
		Bounds, Work rect
		Flags        uint32
	}
	monitor.Size = uint32(unsafe.Sizeof(monitor))
	handle := ucall("MonitorFromWindow", h, 2)
	if ucall("GetMonitorInfoW", handle, uintptr(unsafe.Pointer(&monitor))) != 0 {
		rc.Left = max(monitor.Work.Left, min(rc.Left, monitor.Work.Right-uiWidth()))
		rc.Top = max(monitor.Work.Top, min(rc.Top, monitor.Work.Bottom-uiHeight()))
	}
	ucall("SetWindowPos", h, 0, uintptr(rc.Left), uintptr(rc.Top), uintptr(uiWidth()), uintptr(uiHeight()), 0x14)
}
func closeNativeWindow() {
	if h := nativeWindow.Load(); h != 0 {
		ucall("PostMessageW", h, 0x8002, 0, 0)
	}
}
func toggleNativeWindow() {
	if h := nativeWindow.Load(); h != 0 {
		ucall("PostMessageW", h, 0x8001, 0, 0)
	}
}
func (r *relay) runWindow() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	uiRelay = r
	r.mu.RLock()
	uiExpanded = r.settings.PanelMode == panelFull
	r.mu.RUnlock()
	initialStatus := r.status()
	uiRows = len(panelAccounts(initialStatus))
	uiFooter = len(initialStatus.Accounts) > 3 || initialStatus.Error != ""
	ucall("SetProcessDpiAwarenessContext", ^uintptr(3))
	dpi := ucall("GetDpiForSystem")
	if dpi > 0 {
		uiScale = float64(dpi) / 96
	}
	inst, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	icon := ticketIconRGBA()
	uiIcon = ucall("CreateIconFromResourceEx", uintptr(unsafe.Pointer(&icon[22])), uintptr(len(icon)-22), 1, 0x30000, 32, 32, 0)
	wc := wndClass{Size: uint32(unsafe.Sizeof(wndClass{})), Proc: wndProcCallback, Instance: inst, Name: wide("CPAStateNative"), Cursor: ucall("LoadCursorW", 0, 32512), Icon: uiIcon, SmallIcon: uiIcon}
	if ucall("RegisterClassExW", uintptr(unsafe.Pointer(&wc))) == 0 {
		r.cancel()
		return
	}
	area := rect{}
	ucall("SystemParametersInfoW", 0x30, 0, uintptr(unsafe.Pointer(&area)), 0)
	x, y := area.Right-uiWidth()-scaled(24), area.Top+scaled(24)
	r.mu.RLock()
	prefs := r.settings
	r.mu.RUnlock()
	if prefs.PositionSaved {
		x = prefs.WindowX
		y = prefs.WindowY
	}
	vx := int32(ucall("GetSystemMetrics", 76))
	vy := int32(ucall("GetSystemMetrics", 77))
	vw := int32(ucall("GetSystemMetrics", 78))
	vh := int32(ucall("GetSystemMetrics", 79))
	if x < vx || x+uiWidth() > vx+vw || y < vy || y+uiHeight() > vy+vh {
		x = area.Right - uiWidth() - scaled(24)
		y = area.Top + scaled(24)
	}
	// TOOLWINDOW + TOPMOST, borderless compact native status panel.
	h := ucall("CreateWindowExW", 0x88, uintptr(unsafe.Pointer(wc.Name)), uintptr(unsafe.Pointer(wide("CPA State"))), 0x80000000, uintptr(x), uintptr(y), uintptr(uiWidth()), uintptr(uiHeight()), 0, 0, inst, 0)
	if h == 0 {
		r.cancel()
		return
	}
	nativeWindow.Store(h)
	createPanelTooltip(h)
	updatePanelTooltip(h, initialStatus)
	ucall("SetTimer", h, 1, 1000, 0)
	ucall("ShowWindow", h, 4)
	ucall("UpdateWindow", h)
	var msg winMsg
	for {
		v := ucall("GetMessageW", uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if v == 0 || v == ^uintptr(0) {
			break
		}
		ucall("TranslateMessage", uintptr(unsafe.Pointer(&msg)))
		ucall("DispatchMessageW", uintptr(unsafe.Pointer(&msg)))
	}
	nativeWindow.Store(0)
	if uiIcon != 0 {
		ucall("DestroyIcon", uiIcon)
	}
}
func windowProc(h uintptr, msg uint32, w, l uintptr) uintptr {
	switch msg {
	case 0xf:
		ps := paintStruct{}
		dc := ucall("BeginPaint", h, uintptr(unsafe.Pointer(&ps)))
		renderPanel(dc, uiRelay.status())
		ucall("EndPaint", h, uintptr(unsafe.Pointer(&ps)))
		return 0
	case 0x14:
		return 1
	case 0x113:
		resizePanel(h)
		updatePanelTooltip(h, uiRelay.status())
		ucall("InvalidateRect", h, 0, 0)
		if detailWindow != 0 && ucall("IsWindowVisible", detailWindow) != 0 {
			refreshDetails()
		}
		return 0
	case 0x201:
		x := int(float64(int16(l&0xffff)) / uiScale)
		y := int(float64(int16((l>>16)&0xffff)) / uiScale)
		s := uiRelay.status()
		rows := panelAccounts(s)
		layout := makePanelLayout(uiExpanded, len(rows), len(s.Accounts) > 3 || s.Error != "")
		action, index := layout.hit(x, y, len(s.Accounts) > 3)
		applyPanelAction(h, action, index, s)
		return 0
	case 0x7b:
		showPanelMenu(h, l)
		return 0
	case 0x232:
		rc := rect{}
		ucall("GetWindowRect", h, uintptr(unsafe.Pointer(&rc)))
		uiRelay.mu.Lock()
		uiRelay.settings.WindowX = rc.Left
		uiRelay.settings.WindowY = rc.Top
		uiRelay.settings.PositionSaved = true
		uiRelay.mu.Unlock()
		go func() { uiRelay.report(uiRelay.saveSettings()) }()
		return 0
	case 0x2e0:
		uiScale = float64(w&0xffff) / 96
		rc := rect{}
		kernel32.NewProc("RtlMoveMemory").Call(uintptr(unsafe.Pointer(&rc)), l, unsafe.Sizeof(rc))
		ucall("SetWindowPos", h, 0, uintptr(rc.Left), uintptr(rc.Top), uintptr(uiWidth()), uintptr(uiHeight()), 0x14)
		resizePanel(h, true)
		updatePanelTooltip(h, uiRelay.status())
		return 0
	case 0x10:
		ucall("ShowWindow", h, 0)
		return 0
	case 0x8001:
		if ucall("IsWindowVisible", h) == 0 {
			ucall("ShowWindow", h, 4)
		} else {
			ucall("ShowWindow", h, 0)
		}
		return 0
	case 0x8002:
		ucall("DestroyWindow", h)
		return 0
	case 2:
		panelTooltip = 0
		panelTips = nil
		ucall("PostQuitMessage", 0)
		return 0
	}
	return ucall("DefWindowProcW", h, uintptr(msg), w, l)
}

func applyPanelAction(h uintptr, action panelAction, index int, s statusView) {
	rows := panelAccounts(s)
	switch action {
	case panelHide:
		ucall("ShowWindow", h, 0)
	case panelShowCompact, panelShowFull:
		full := action == panelShowFull
		if uiExpanded != full {
			uiExpanded = full
			uiRelay.report(uiRelay.setPanelMode(full))
			resizePanel(h, true)
			updatePanelTooltip(h, s)
			ucall("InvalidateRect", h, 0, 0)
		}
	case panelToggleBPS:
		row := rows[index]
		uiRelay.accountsMu.RLock()
		target := uiRelay.accounts[row.ID]
		uiRelay.accountsMu.RUnlock()
		if target != nil {
			go func() { target.report(target.setBPS(!row.State.BPS)) }()
		}
	case panelDetails:
		if rows[index].ID != "" {
			showSessionDetails(rows[index].ID)
		}
	case panelNextPage:
		uiPage = (uiPage + 1) % ((len(s.Accounts) + 2) / 3)
		resizePanel(h)
		updatePanelTooltip(h, s)
		ucall("InvalidateRect", h, 0, 0)
	case panelDrag:
		ucall("ReleaseCapture")
		ucall("SendMessageW", h, 0xa1, 2, 0)
	}
}
func showPanelMenu(h, position uintptr) {
	s := uiRelay.status()
	menu := ucall("CreatePopupMenu")
	if menu == 0 {
		return
	}
	defer ucall("DestroyMenu", menu)
	for _, item := range []struct {
		action  panelAction
		text    string
		checked bool
	}{
		{panelShowCompact, "简约模式", !uiExpanded},
		{panelShowFull, "完整模式", uiExpanded},
	} {
		flags := uintptr(0)
		if item.checked {
			flags = 8
		}
		ucall("AppendMenuW", menu, flags, uintptr(item.action), uintptr(unsafe.Pointer(wide(item.text))))
	}
	if len(s.Accounts) > 3 {
		ucall("AppendMenuW", menu, 0, uintptr(panelNextPage), uintptr(unsafe.Pointer(wide(fmt.Sprintf("下一页（%d / %d）", uiPage+1, (len(s.Accounts)+2)/3)))))
	}
	if s.Error != "" {
		ucall("AppendMenuW", menu, 2, 0, uintptr(unsafe.Pointer(wide(s.Error))))
	}
	ucall("AppendMenuW", menu, 0x800, 0, 0)
	ucall("AppendMenuW", menu, 0, uintptr(panelHide), uintptr(unsafe.Pointer(wide("隐藏窗口"))))
	x, y := int32(int16(position&0xffff)), int32(int16(position>>16&0xffff))
	if x == -1 && y == -1 {
		rc := rect{}
		ucall("GetWindowRect", h, uintptr(unsafe.Pointer(&rc)))
		x, y = rc.Left+scaled(12), rc.Top+scaled(12)
	}
	command := ucall("TrackPopupMenu", menu, 0x102, uintptr(x), uintptr(y), 0, h, 0)
	if command != 0 {
		applyPanelAction(h, panelAction(command), -1, s)
	}
}

func ticketIconRGBA() []byte { return ticketIcon(iconBlue) }
func fill(dc uintptr, x, y, w, h int, c uint32) {
	rc := rect{scaled(x), scaled(y), scaled(x + w), scaled(y + h)}
	brush := gcall("CreateSolidBrush", uintptr(c))
	ucall("FillRect", dc, uintptr(unsafe.Pointer(&rc)), brush)
	gcall("DeleteObject", brush)
}
func label(dc uintptr, text string, x, y, w, h, size int, c uint32, bold bool) {
	weight := 400
	if bold {
		weight = 600
	}
	height := -scaled(size)
	font := gcall("CreateFontW", uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(wide("Microsoft YaHei UI"))))
	old := gcall("SelectObject", dc, font)
	gcall("SetBkMode", dc, 1)
	gcall("SetTextColor", dc, uintptr(c))
	rc := rect{scaled(x), scaled(y), scaled(x + w), scaled(y + h)}
	t, _ := syscall.UTF16FromString(text)
	ucall("DrawTextW", dc, uintptr(unsafe.Pointer(&t[0])), uintptr(len(t)-1), uintptr(unsafe.Pointer(&rc)), 0x8024)
	gcall("SelectObject", dc, old)
	gcall("DeleteObject", font)
}
func panelButton(dc uintptr, x, y, w int, text string, on bool) {
	bg, fg := uint32(0x44392f), uint32(0xd6dfe9)
	if on {
		bg = 0x4b4730
		fg = 0xa4ddb0
	}
	fill(dc, x, y, w, 27, bg)
	label(dc, text, x+10, y+3, w-18, 21, 12, fg, false)
}
func panelTextColor(severity panelSeverity) uint32 {
	switch severity {
	case panelError:
		return 0x9998ef
	case panelWarning:
		return 0x80caef
	default:
		return 0xd6dfe9
	}
}
func drawPanelButton(dc uintptr, b panelBox, text string, active bool) {
	bg, fg := uint32(0x44392f), uint32(0xd6dfe9)
	if active {
		bg, fg = 0x4b4730, 0xa4ddb0
	}
	fill(dc, b.X, b.Y, b.W, b.H, bg)
	label(dc, text, b.X+10, b.Y+3, b.W-18, b.H-5, 12, fg, false)
}
func renderPanel(dc uintptr, s statusView) {
	rows := panelAccounts(s)
	l := makePanelLayout(uiExpanded, len(rows), len(s.Accounts) > 3 || s.Error != "")
	fill(dc, 0, 0, l.Width, l.Height, 0x211b16)

	if l.Full {
		label(dc, "CPA State", 20, 12, 180, 26, 19, 0xf3eee7, true)
		drawPanelButton(dc, l.modeBox(false), "简约", false)
		drawPanelButton(dc, l.modeBox(true), "完整", true)
		c := l.closeBox()
		label(dc, "×", c.X, c.Y, c.W, c.H, 19, 0xa8a095, false)
		summary := fmt.Sprintf("%d 个账号 · 并发 %d · 等待 %d", len(s.Accounts), s.Active, s.Waiting)
		label(dc, summary, 20, 44, 240, 20, 12, 0xa8a095, false)
	}
	for i, a := range rows {
		b := l.rowBox(i)
		fill(dc, b.X, b.Y, b.W, b.H, 0x30271f)
		result := accountPanelResult(a)
		name := a.displayName()
		if a.ID == "" && !a.Bound {
			name = "暂无账号"
		}
		n := l.nameBox(i)
		size := 13
		if l.Full {
			size = 16
		}
		label(dc, name, n.X, n.Y, n.W, n.H, size, 0xf3eee7, true)
		if a.ID == "" && !a.Bound {
			x, y := 110, b.Y+6
			if l.Full {
				x, y = 26, b.Y+42
			}
			label(dc, "等待登录资料或首次请求", x, y, l.Width-x-26, 24, 12, 0xa8a095, false)
			continue
		}
		bps := "BPS 关"
		if a.State.BPS {
			bps = "BPS 开"
		}
		drawPanelButton(dc, l.toggleBox(i), bps, a.State.BPS)
		r := l.resultBox(i)
		d := l.detailBox(i)
		if !l.Full {
			c := l.countBox(i)
			label(dc, fmt.Sprintf("并%d 等%d", a.State.Active, a.State.Waiting), c.X, c.Y, c.W, c.H, 12, 0xa8a095, false)
			label(dc, result.Text, r.X, r.Y, r.W, r.H, 12, panelTextColor(result.Severity), false)
			label(dc, "›", d.X, d.Y, d.W, d.H, 19, 0xa8a095, false)
			if i < len(rows)-1 {
				fill(dc, b.X+12, b.Y+b.H-1, b.W-24, 1, 0x3d332b)
			}
			continue
		}
		fill(dc, 14, b.Y+12, 3, 122, panelTextColor(result.Severity))
		email := a.Email
		if email == "" {
			email = "暂无邮箱 · " + a.ID
		}
		label(dc, email, 26, b.Y+36, 410, 20, 12, 0xa8a095, false)
		latest := a.Latest.text()
		if result.Severity == panelError {
			latest = result.Text
		}
		if !a.Bound {
			latest = "未绑定 · " + latest
		}
		label(dc, "最近转发  "+latest, r.X, r.Y, r.W, r.H, 14, panelTextColor(result.Severity), false)
		label(dc, a.Latest.At, 485, b.Y+64, 86, 21, 12, 0xa8a095, false)
		stats := fmt.Sprintf("通过 %d  ·  未通过 %d  ·  待判定 %d  ·  转发 %d", a.State.Passed, a.State.Failed, a.State.Pending, a.State.Forwarded)
		label(dc, stats, 26, b.Y+89, 545, 21, 12, 0xa8a095, false)
		drawPanelButton(dc, d, "会话详情", false)
		if a.Latest.effortText() != "" {
			label(dc, a.Latest.effortText(), 166, b.Y+119, 406, 21, 12, panelTextColor(result.Severity), false)
		}
	}
	bottom := l.Top + len(rows)*l.Step
	if l.Full {
		note := "按账号切换通道 · 仅影响新请求"
		color := uint32(0xa8a095)
		if s.Error != "" {
			note = s.Error
			color = panelTextColor(panelError)
		}
		w := l.Width - 40
		if len(s.Accounts) > 3 {
			w = l.Width - 188
		}
		label(dc, note, 20, bottom+8, w, 22, 11, color, false)
	}
	if l.Full && len(s.Accounts) > 3 {
		p := l.pageBox()
		label(dc, fmt.Sprintf("%d / %d  下一页 ›", uiPage+1, (len(s.Accounts)+2)/3), p.X, p.Y, p.W, p.H, 12, 0xcbb38c, false)
	}
}
