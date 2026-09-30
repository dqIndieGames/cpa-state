package main

import (
	"fmt"
	"strings"
)

const (
	panelCompact = "compact"
	panelFull    = "full"
)

type panelBox struct{ X, Y, W, H int }

func (b panelBox) contains(x, y int) bool {
	return x >= b.X && x < b.X+b.W && y >= b.Y && y < b.Y+b.H
}

type panelLayout struct {
	Full                           bool
	Width, Height, Top, Step, Rows int
}

func makePanelLayout(full bool, rows int, footer bool) panelLayout {
	l := panelLayout{Full: full, Width: 460, Top: 4, Step: 36, Rows: rows}
	l.Height = 8 + rows*l.Step
	if full {
		l.Width, l.Top, l.Step = 600, 72, 154
		l.Height = 116 + rows*l.Step
	}
	return l
}

func (l panelLayout) closeBox() panelBox { return panelBox{l.Width - 36, 12, 24, 26} }
func (l panelLayout) modeBox(full bool) panelBox {
	x := l.Width - 152
	if full {
		x += 50
	}
	return panelBox{x, 14, 48, 25}
}
func (l panelLayout) rowBox(i int) panelBox {
	h := l.Step
	if l.Full {
		h -= 8
		return panelBox{14, l.Top + i*l.Step, l.Width - 28, h}
	}
	return panelBox{4, l.Top + i*l.Step, l.Width - 8, h}
}
func (l panelLayout) nameBox(i int) panelBox {
	if l.Full {
		return panelBox{26, l.Top + i*l.Step + 10, 410, 24}
	}
	return panelBox{12, l.Top + i*l.Step + 6, 94, 24}
}
func (l panelLayout) countBox(i int) panelBox {
	return panelBox{110, l.Top + i*l.Step + 6, 82, 24}
}
func (l panelLayout) toggleBox(i int) panelBox {
	if l.Full {
		return panelBox{454, l.Top + i*l.Step + 14, 118, 27}
	}
	return panelBox{196, l.Top + i*l.Step + 5, 74, 26}
}
func (l panelLayout) resultBox(i int) panelBox {
	if l.Full {
		return panelBox{26, l.Top + i*l.Step + 62, 448, 24}
	}
	return panelBox{280, l.Top + i*l.Step + 6, 144, 24}
}
func (l panelLayout) detailBox(i int) panelBox {
	if l.Full {
		return panelBox{26, l.Top + i*l.Step + 116, 124, 27}
	}
	return panelBox{l.Width - 26, l.Top + i*l.Step + 5, 22, 26}
}
func (l panelLayout) pageBox() panelBox {
	return panelBox{l.Width - 156, l.Top + l.Rows*l.Step + 6, 136, 24}
}

type panelAction int

const (
	panelNone panelAction = iota
	panelHide
	panelShowCompact
	panelShowFull
	panelToggleBPS
	panelDetails
	panelNextPage
	panelDrag
)

func (l panelLayout) hit(x, y int, pages bool) (panelAction, int) {
	if l.Full {
		if l.closeBox().contains(x, y) {
			return panelHide, -1
		}
		if l.modeBox(false).contains(x, y) {
			return panelShowCompact, -1
		}
		if l.modeBox(true).contains(x, y) {
			return panelShowFull, -1
		}
	}
	for i := 0; i < l.Rows; i++ {
		if l.toggleBox(i).contains(x, y) {
			return panelToggleBPS, i
		}
		if l.nameBox(i).contains(x, y) || l.detailBox(i).contains(x, y) {
			return panelDetails, i
		}
	}
	if l.Full && pages && l.pageBox().contains(x, y) {
		return panelNextPage, -1
	}
	if y >= 0 && (y < l.Top || !l.Full && y < l.Height) && x >= 0 && x < l.Width {
		return panelDrag, -1
	}
	return panelNone, -1
}

type panelSeverity int

const (
	panelNormal panelSeverity = iota
	panelWarning
	panelError
)

type panelResult struct {
	Text     string
	Severity panelSeverity
}

func accountPanelResult(a accountView) panelResult {
	t := a.Latest
	if t.Error != "" {
		return panelResult{"错误 · " + strings.Join(strings.Fields(t.Error), " "), panelError}
	}
	if a.State.Error != "" {
		return panelResult{"错误 · " + strings.Join(strings.Fields(a.State.Error), " "), panelError}
	}
	if t.Responded && t.HTTP >= 400 {
		return panelResult{fmt.Sprintf("错误 · HTTP %d", t.HTTP), panelError}
	}
	if t.Responded && !t.BPS && t.ResponseLength > 0 && !goodTicketLength(t.ResponseLength) {
		return panelResult{"异常 · " + t.text(), panelError}
	}
	if !a.Bound {
		return panelResult{"未绑定", panelWarning}
	}
	if t.BPS && t.ActualEffort != "" && t.RequestedEffort != t.ActualEffort {
		return panelResult{"降级至 " + t.ActualEffort, panelWarning}
	}
	if !t.Dispatched {
		return panelResult{"尚无转发", panelNormal}
	}
	if !t.Responded {
		return panelResult{"等待响应", panelNormal}
	}
	if t.BPS {
		text := fmt.Sprint(t.HTTP)
		if t.ActualEffort != "" {
			text += " · " + t.ActualEffort
		}
		return panelResult{text, panelNormal}
	}
	return panelResult{t.text(), panelNormal}
}

func accountPanelHint(a accountView) string {
	lines := []string{a.displayName()}
	lines = append(lines, fmt.Sprintf("并发 %d · 等待 %d", a.State.Active, a.State.Waiting))
	if a.Email != "" {
		lines = append(lines, a.Email)
	}
	lines = append(lines, "最近转发："+a.Latest.text())
	if a.Latest.At != "" {
		lines = append(lines, "时间："+a.Latest.At)
	}
	if a.Latest.ActualEffort != "" {
		lines = append(lines, a.Latest.effortText())
	}
	if a.State.Error != "" {
		lines = append(lines, a.State.Error)
	}
	return strings.Join(lines, "\n")
}

func (r *relay) setPanelMode(full bool) error {
	mode := panelCompact
	if full {
		mode = panelFull
	}
	r.mu.Lock()
	r.settings.PanelMode = mode
	r.mu.Unlock()
	return r.saveSettings()
}
