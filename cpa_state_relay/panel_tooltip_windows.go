package main

import (
	"syscall"
	"unsafe"
)

var panelTooltip uintptr
var panelTips []panelTip

type panelTip struct {
	Box     panelBox
	Content string
	UTF16   []uint16
}
type panelToolInfo struct {
	Size, Flags uint32
	Window, ID  uintptr
	Bounds      rect
	Instance    uintptr
	Text        *uint16
	// TOOLINFO V2 also works without a Common Controls v6 manifest.
	Param uintptr
}

func panelTool(h uintptr, i int, tip *panelTip) panelToolInfo {
	b := tip.Box
	return panelToolInfo{Size: uint32(unsafe.Sizeof(panelToolInfo{})), Flags: 0x10, Window: h, ID: uintptr(i + 1), Bounds: rect{scaled(b.X), scaled(b.Y), scaled(b.X + b.W), scaled(b.Y + b.H)}, Text: &tip.UTF16[0]}
}
func createPanelTooltip(h uintptr) {
	init := struct{ Size, Classes uint32 }{8, 0xff}
	syscall.NewLazyDLL("comctl32.dll").NewProc("InitCommonControlsEx").Call(uintptr(unsafe.Pointer(&init)))
	panelTooltip = ucall("CreateWindowExW", 0x88, uintptr(unsafe.Pointer(wide("tooltips_class32"))), 0, 0x80000003, 0, 0, 0, 0, h, 0, 0, 0)
	if panelTooltip != 0 {
		margins := rect{scaled(8), scaled(6), scaled(8), scaled(6)}
		ucall("SendMessageW", panelTooltip, 0x41a, 0, uintptr(unsafe.Pointer(&margins)))
		ucall("SendMessageW", panelTooltip, 0x418, 0, uintptr(scaled(420)))
		ucall("SendMessageW", panelTooltip, 0x403, 3, 450)
		ucall("SendMessageW", panelTooltip, 0x403, 2, 15000)
	}
}
func updatePanelTooltip(h uintptr, s statusView) {
	if panelTooltip == 0 {
		return
	}
	rows := panelAccounts(s)
	l := makePanelLayout(uiExpanded, len(rows), len(s.Accounts) > 3 || s.Error != "")
	var next []panelTip
	for i, a := range rows {
		if a.ID == "" && !a.Bound {
			continue
		}
		name, result := l.nameBox(i), l.resultBox(i)
		if l.Full {
			name.H = 46
			result.H = 49
		}
		hint := accountPanelHint(a)
		if !l.Full && s.Error != "" {
			hint += string(rune(10)) + s.Error
		}
		if !l.Full {
			next = append(next, panelTip{Box: l.countBox(i), Content: hint})
		}
		next = append(next, panelTip{Box: name, Content: hint}, panelTip{Box: result, Content: hint})
	}
	if l.Full && s.Error != "" {
		w := l.Width - 40
		if len(s.Accounts) > 3 {
			w = l.Width - 188
		}
		next = append(next, panelTip{Box: panelBox{20, l.Top + len(rows)*l.Step + 6, w, 24}, Content: s.Error})
	}
	for i := len(next); i < len(panelTips); i++ {
		info := panelTool(h, i, &panelTips[i])
		ucall("SendMessageW", panelTooltip, 0x433, 0, uintptr(unsafe.Pointer(&info)))
	}
	for i := range next {
		next[i].UTF16, _ = syscall.UTF16FromString(next[i].Content)
		if len(next[i].UTF16) == 0 {
			next[i].UTF16 = []uint16{0}
		}
		info := panelTool(h, i, &next[i])
		if i >= len(panelTips) {
			ucall("SendMessageW", panelTooltip, 0x432, 0, uintptr(unsafe.Pointer(&info)))
		} else {
			ucall("SendMessageW", panelTooltip, 0x434, 0, uintptr(unsafe.Pointer(&info)))
			if next[i].Content != panelTips[i].Content {
				ucall("SendMessageW", panelTooltip, 0x439, 0, uintptr(unsafe.Pointer(&info)))
			} else {
				next[i].UTF16 = panelTips[i].UTF16
			}
		}
	}
	panelTips = next
}
