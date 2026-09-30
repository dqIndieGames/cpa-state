package main

import (
	"os"
	"strings"
	"testing"
)

// L1 invariants: changing presentation must preserve account/routing state,
// every control must stay within its row, and hit regions must not overlap.
func TestPanelModePreservesAccountAndRelayState(t *testing.T) {
	r, other := accountFixture(t)
	r.settings.BPS = true
	r.settings.TicketWrite = true
	r.settings.WindowX = 120
	r.settings.WindowY = 80
	r.settings.PositionSaved = true
	before := r.settings
	otherBefore := other.settings
	beforeEpoch := r.epoch
	for _, full := range []bool{true, false, true} {
		if err := r.setPanelMode(full); err != nil {
			t.Fatal(err)
		}
		after := r.settings
		after.PanelMode = before.PanelMode
		if after != before || other.settings != otherBefore || r.epoch != beforeEpoch {
			t.Fatal("presentation changed routing, account settings, position or epoch")
		}
		reloaded := testRelay(t)
		reloaded.settingsFile = r.settingsFile
		reloaded.loadSettings()
		if (reloaded.settings.PanelMode == panelFull) != full {
			t.Fatal("selected mode did not survive reload")
		}
	}
	legacy := testRelay(t)
	legacyJSON := []byte("{\"bps_enabled\":true,\"position_saved\":true}")
	if err := os.WriteFile(legacy.settingsFile, legacyJSON, 0600); err != nil {
		t.Fatal(err)
	}
	legacy.loadSettings()
	if legacy.settings.PanelMode == panelFull || !legacy.settings.BPS || !legacy.settings.PositionSaved {
		t.Fatal("legacy preferences did not retain routing and default compact mode")
	}
}

func TestPanelHitRegionsRemainWithinRowsAndSeparateActions(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, count := range []int{1, 2, 3} {
			l := makePanelLayout(full, count, true)
			controls := []struct {
				box    panelBox
				action panelAction
				row    int
			}{
				{l.closeBox(), panelHide, -1},
				{l.modeBox(false), panelShowCompact, -1},
				{l.modeBox(true), panelShowFull, -1},
				{l.pageBox(), panelNextPage, -1},
			}
			// DEV-rendered compact panel has account rows only; header and
			// footer controls exist only in full mode. Keep the same bounds and
			// non-overlap invariants for every visible control in either mode.
			if !full {
				controls = controls[:0]
			}
			for i := 0; i < count; i++ {
				for _, v := range []struct {
					b panelBox
					a panelAction
				}{
					{l.toggleBox(i), panelToggleBPS}, {l.nameBox(i), panelDetails}, {l.detailBox(i), panelDetails},
				} {
					r := l.rowBox(i)
					if !r.contains(v.b.X, v.b.Y) || !r.contains(v.b.X+v.b.W-1, v.b.Y+v.b.H-1) {
						t.Fatal("control outside account row")
					}
					controls = append(controls, struct {
						box    panelBox
						action panelAction
						row    int
					}{v.b, v.a, i})
				}
			}
			for _, c := range controls {
				if c.box.X < 0 || c.box.Y < 0 || c.box.X+c.box.W > l.Width || c.box.Y+c.box.H > l.Height {
					t.Fatal("control outside client area")
				}
				for x := c.box.X; x < c.box.X+c.box.W; x++ {
					for y := c.box.Y; y < c.box.Y+c.box.H; y++ {
						action, row := l.hit(x, y, true)
						if action != c.action || row != c.row {
							t.Fatal("overlapping control or wrong account hit")
						}
					}
				}
			}
		}
	}
	compact, full := makePanelLayout(false, 2, false), makePanelLayout(true, 2, false)
	if compact.Height >= full.Height || compact.Width >= full.Width {
		t.Fatal("compact mode did not reduce window footprint")
	}
}

func TestPanelResultsPrioritizeFailuresAndKeepHistorySeparateFromRoute(t *testing.T) {
	a := accountView{Bound: true, State: statusView{BPS: true}, Latest: transferView{Dispatched: true, Responded: true, BPS: true, HTTP: 200, RequestedEffort: "max", ActualEffort: "xhigh"}}
	if result := accountPanelResult(a); result.Severity != panelWarning || !strings.Contains(result.Text, a.Latest.ActualEffort) {
		t.Fatal("downgrade is not visible")
	}
	a.Latest.HTTP = 429
	if result := accountPanelResult(a); result.Severity != panelError {
		t.Fatal("HTTP failure was hidden by downgrade")
	}
	a.Latest.Error = "upstream stream failed"
	if result := accountPanelResult(a); result.Severity != panelError || !strings.Contains(result.Text, a.Latest.Error) {
		t.Fatal("stream error was hidden")
	}
	a.Latest.Error = ""
	a.Latest.HTTP = 200
	a.Latest.RequestedEffort = a.Latest.ActualEffort
	before := accountPanelResult(a)
	a.State.BPS = false
	if after := accountPanelResult(a); after != before {
		t.Fatal("current route rewrote historical result")
	}
	a.State.Error = "settings could not be saved"
	if result := accountPanelResult(a); result.Severity != panelError || !strings.Contains(result.Text, a.State.Error) {
		t.Fatal("account error was hidden by normal result")
	}
	a.State.Error = ""
	a.Bound = false
	if result := accountPanelResult(a); result.Severity != panelWarning {
		t.Fatal("unbound account was presented as normal")
	}
	a.Name = strings.Repeat("Long account ", 12)
	a.Email = "fixture@example.test"
	hint := accountPanelHint(a)
	if !strings.Contains(hint, a.Name) || !strings.Contains(hint, a.Email) {
		t.Fatal("hover hint lost full identity")
	}
}
