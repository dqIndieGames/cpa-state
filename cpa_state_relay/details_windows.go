package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var detailWindow, detailList, detailEdit, detailCopy, detailSummary, detailFont uintptr
var detailRows []sessionView
var detailAccount string
var detailListText, detailBodyText string
var detailScale = 1.0
var detailEffortWarning bool
var detailErrorToggle uintptr
var detailErrorsExpanded bool
var detailProcCallback = syscall.NewCallback(detailsProc)

func showSessionDetails(accountIDs ...string) {
	if len(accountIDs) > 0 {
		detailAccount = accountIDs[0]
	}
	if detailWindow != 0 {
		ucall("ShowWindow", detailWindow, 4)
		refreshDetails()
		return
	}
	inst, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	wc := wndClass{Size: uint32(unsafe.Sizeof(wndClass{})), Proc: detailProcCallback, Instance: inst, Name: wide("CPAStateSessions"), Cursor: ucall("LoadCursorW", 0, 32512), Background: 16, Icon: uiIcon, SmallIcon: uiIcon}
	ucall("RegisterClassExW", uintptr(unsafe.Pointer(&wc)))
	detailScale = uiScale
	d := func(n int) uintptr { return uintptr(float64(n) * detailScale) }
	h := ucall("CreateWindowExW", 0, uintptr(unsafe.Pointer(wc.Name)), uintptr(unsafe.Pointer(wide("CPA State · 会话观测详情"))), 0x00cf0000, 100, 100, d(820), d(640), nativeWindow.Load(), 0, inst, 0)
	if h == 0 {
		return
	}
	detailWindow = h
	makeControl := func(class, text string, style uintptr, id uintptr) uintptr {
		return ucall("CreateWindowExW", 0, uintptr(unsafe.Pointer(wide(class))), uintptr(unsafe.Pointer(wide(text))), 0x50000000|style, 0, 0, 10, 10, h, id, inst, 0)
	}
	detailSummary = makeControl("STATIC", "", 0, 100)
	detailList = makeControl("LISTBOX", "", 0x00a00101, 101) // border, vertical scroll, notify, no integral height
	detailEdit = makeControl("EDIT", "", 0x00a00844, 102)    // read-only multiline, auto vertical scroll, wrapping
	ucall("SendMessageW", detailEdit, 0xc5, 32768, 0)
	detailCopy = makeControl("BUTTON", "复制 Session", 0x10000, 103)
	detailErrorToggle = makeControl("BUTTON", "展开原因", 0x10000, 104)
	setDetailsFont()
	layoutDetails()
	refreshDetails()
	ucall("ShowWindow", h, 4)
	ucall("UpdateWindow", h)
}
func setDetailsFont() {
	height := -int32(14 * detailScale)
	old := detailFont
	detailFont = gcall("CreateFontW", uintptr(height), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(wide("Microsoft YaHei UI"))))
	for _, h := range []uintptr{detailSummary, detailList, detailEdit, detailCopy, detailErrorToggle} {
		ucall("SendMessageW", h, 0x30, detailFont, 1)
	}
	if old != 0 {
		gcall("DeleteObject", old)
	}
}
func layoutDetails() {
	if detailWindow == 0 {
		return
	}
	rc := rect{}
	ucall("GetClientRect", detailWindow, uintptr(unsafe.Pointer(&rc)))
	d := func(n int) int32 { return int32(float64(n) * detailScale) }
	width, height := rc.Right, rc.Bottom
	pad := d(14)
	listHeight := max(d(90), (height-d(160))*2/5)
	move := func(h uintptr, x, y, w, height int32) {
		ucall("MoveWindow", h, uintptr(x), uintptr(y), uintptr(max(1, w)), uintptr(max(1, height)), 1)
	}
	move(detailSummary, pad, pad, width-pad*2, d(70))
	move(detailList, pad, d(92), width-pad*2, listHeight)
	editY := d(104) + listHeight
	move(detailEdit, pad, editY, width-pad*2, height-editY-d(56))
	move(detailCopy, width-pad-d(140), height-d(42), d(140), d(28))
	move(detailErrorToggle, pad, height-d(42), d(140), d(28))
}
func selectedDetail() *sessionView {
	index := int(int32(ucall("SendMessageW", detailList, 0x188, 0, 0)))
	if index < 0 || index >= len(detailRows) {
		return nil
	}
	return &detailRows[index]
}

// Fit using the same font and actual client width as the native list. The full
// title and source remain available in the wrapping read-only detail control.
func fitDetailLine(line string) string {
	rc := rect{}
	ucall("GetClientRect", detailList, uintptr(unsafe.Pointer(&rc)))
	dc := ucall("GetDC", detailList)
	if dc == 0 {
		return boundedText(line, 60)
	}
	defer ucall("ReleaseDC", detailList, dc)
	old := gcall("SelectObject", dc, detailFont)
	defer gcall("SelectObject", dc, old)
	fits := func(text string) bool {
		encoded, _ := syscall.UTF16FromString(text)
		var size point
		gcall("GetTextExtentPoint32W", dc, uintptr(unsafe.Pointer(&encoded[0])), uintptr(len(encoded)-1), uintptr(unsafe.Pointer(&size)))
		return size.X <= rc.Right-int32(12*detailScale)
	}
	if fits(line) {
		return line
	}
	runes := []rune(line)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if fits(string(runes[:mid]) + "…") {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low]) + "…"
}
func refreshDetailBody() {
	body := "暂无会话。正常 Codex 请求经过本中转后，会在这里显示。"
	row := selectedDetail()
	if row != nil {
		body = row.detailTextExpanded(detailErrorsExpanded)
	}
	if body != detailBodyText {
		ucall("SetWindowTextW", detailEdit, uintptr(unsafe.Pointer(wide(body))))
		detailBodyText = body
	}
	enabled := uintptr(0)
	if row != nil {
		enabled = 1
	}
	ucall("EnableWindow", detailCopy, enabled)
	errorEnabled := uintptr(0)
	if row != nil && len(row.Errors) > 0 {
		errorEnabled = 1
	}
	ucall("EnableWindow", detailErrorToggle, errorEnabled)
}
func refreshDetails() {
	if detailWindow == 0 {
		return
	}
	s := uiRelay.status()
	accountTitle := "全部账号"
	for _, a := range s.Accounts {
		if a.ID == detailAccount {
			s = a.State
			accountTitle = a.displayName() + " · " + a.Email
			break
		}
	}
	ucall("SetWindowTextW", detailWindow, uintptr(unsafe.Pointer(wide("CPA State · "+accountTitle))))
	summary := fmt.Sprintf("通过 %d  ·  未通过 %d  ·  待判定 %d    最近 %d 个会话（本账号最多500） · 无ID请求 %d", s.Passed, s.Failed, s.Pending, len(s.Sessions), s.Unidentified)
	channel := "原通道"
	if s.BPS {
		channel = "BPS"
	}
	summary += "\r\n当前通道：" + channel + " · 签发功能暂时废弃"
	info := "通道开关仅影响新请求"
	detailEffortWarning = false
	for _, a := range uiRelay.status().Accounts {
		if a.ID == detailAccount && s.BPS && a.Latest.effortText() != "" {
			info = a.Latest.effortText()
			detailEffortWarning = a.Latest.RequestedEffort != a.Latest.ActualEffort
		}
	}
	if s.Error != "" {
		info = s.Error
	}
	summary += "\r\n" + boundedText(info, 140)
	ucall("SetWindowTextW", detailSummary, uintptr(unsafe.Pointer(wide(summary))))
	selected := ""
	if row := selectedDetail(); row != nil {
		selected = row.AccountID + "\x00" + row.ID
	}
	var lines []string
	for _, row := range s.Sessions {
		title := strings.Join(strings.Fields(row.Title), " ")
		label := row.Status
		if len(row.Errors) > 0 {
			e := row.Errors[0]
			state := "错误"
			if e.RecoveredAt != "" {
				state = "已恢复"
			}
			label = fmt.Sprintf("%s %s %d ×%d", state, e.Mode, e.HTTP, e.Count)
		}
		lines = append(lines, fitDetailLine(fmt.Sprintf("%s  |  %s  |  %s", label, row.Model, title)))
	}
	joined := strings.Join(lines, "\x00")
	for _, row := range s.Sessions {
		joined += "\x00" + row.AccountID + "\x00" + row.ID
	}
	detailRows = s.Sessions
	if joined != detailListText {
		top := ucall("SendMessageW", detailList, 0x18e, 0, 0)
		ucall("SendMessageW", detailList, 0xb, 0, 0)
		ucall("SendMessageW", detailList, 0x184, 0, 0)
		choice := 0
		for i, line := range lines {
			ucall("SendMessageW", detailList, 0x180, 0, uintptr(unsafe.Pointer(wide(line))))
			if detailRows[i].AccountID+"\x00"+detailRows[i].ID == selected {
				choice = i
			}
		}
		ucall("SendMessageW", detailList, 0x186, uintptr(choice), 0)
		ucall("SendMessageW", detailList, 0x197, top, 0)
		ucall("SendMessageW", detailList, 0xb, 1, 0)
		ucall("InvalidateRect", detailList, 0, 1)
		detailListText = joined
	}
	refreshDetailBody()
}
func copySession(h uintptr, text string) bool {
	data, e := syscall.UTF16FromString(text)
	if e != nil {
		return false
	}
	mem, _, _ := kernel32.NewProc("GlobalAlloc").Call(0x42, uintptr(len(data)*2))
	if mem == 0 {
		return false
	}
	p, _, _ := kernel32.NewProc("GlobalLock").Call(mem)
	if p == 0 {
		kernel32.NewProc("GlobalFree").Call(mem)
		return false
	}
	kernel32.NewProc("RtlMoveMemory").Call(p, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	kernel32.NewProc("GlobalUnlock").Call(mem)
	if ucall("OpenClipboard", h) == 0 {
		kernel32.NewProc("GlobalFree").Call(mem)
		return false
	}
	defer ucall("CloseClipboard")
	if ucall("EmptyClipboard") == 0 || ucall("SetClipboardData", 13, mem) == 0 {
		kernel32.NewProc("GlobalFree").Call(mem)
		return false
	}
	return true
}
func detailsProc(h uintptr, msg uint32, w, l uintptr) uintptr {
	if msg == 0x138 && l == detailSummary && detailEffortWarning {
		gcall("SetTextColor", w, 0x3030d0)
		gcall("SetBkColor", w, ucall("GetSysColor", 15))
		return ucall("GetSysColorBrush", 15)
	}
	switch msg {
	case 5:
		layoutDetails()
		if detailList != 0 {
			refreshDetails()
		}
		return 0
	case 0x24: // MINMAXINFO, ensure the list and full text remain usable when resized.
		var info [10]int32
		kernel32.NewProc("RtlMoveMemory").Call(uintptr(unsafe.Pointer(&info)), l, unsafe.Sizeof(info))
		info[6] = int32(640 * detailScale)
		info[7] = int32(460 * detailScale)
		kernel32.NewProc("RtlMoveMemory").Call(l, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		return 0
	case 0x111:
		if w&0xffff == 104 {
			detailErrorsExpanded = !detailErrorsExpanded
			label := "展开原因"
			if detailErrorsExpanded {
				label = "收起原因"
			}
			ucall("SetWindowTextW", detailErrorToggle, uintptr(unsafe.Pointer(wide(label))))
			refreshDetailBody()
		}
		if w&0xffff == 101 && (w>>16)&0xffff == 1 {
			refreshDetailBody()
		}
		if w&0xffff == 103 {
			if row := selectedDetail(); row != nil {
				if copySession(h, row.ID) {
					ucall("SetWindowTextW", detailCopy, uintptr(unsafe.Pointer(wide("已复制 Session"))))
					ucall("SetTimer", h, 2, 1500, 0)
				}
			}
		}
		return 0
	case 0x113:
		ucall("SetWindowTextW", detailCopy, uintptr(unsafe.Pointer(wide("复制 Session"))))
		ucall("KillTimer", h, 2)
		return 0
	case 0x2e0:
		detailScale = float64(w&0xffff) / 96
		setDetailsFont()
		rc := rect{}
		kernel32.NewProc("RtlMoveMemory").Call(uintptr(unsafe.Pointer(&rc)), l, unsafe.Sizeof(rc))
		ucall("SetWindowPos", h, 0, uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right-rc.Left), uintptr(rc.Bottom-rc.Top), 0x14)
		layoutDetails()
		return 0
	case 0x10:
		ucall("ShowWindow", h, 0)
		return 0
	case 2:
		detailWindow = 0
		detailListText = ""
		detailBodyText = ""
		detailRows = nil
		if detailFont != 0 {
			gcall("DeleteObject", detailFont)
			detailFont = 0
		}
		return 0
	}
	return ucall("DefWindowProcW", h, uintptr(msg), w, l)
}
