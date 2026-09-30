package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/getlantern/systray"
	"image"
	"image/color"
	"image/png"
	"time"
)

// Ticket glyph remains legible at notification-area size; color conveys state.
func ticketIcon(c color.NRGBA) []byte {
	im := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 5; y < 27; y++ {
		for x := 3; x < 29; x++ {
			if (x < 6 || x > 25) && (y < 8 || y > 23) {
				continue
			}
			if (x < 6 || x > 25) && y >= 14 && y <= 17 {
				continue
			}
			im.SetNRGBA(x, y, c)
		}
	}
	ink := color.NRGBA{18, 26, 39, 255}
	for y := 9; y < 23; y++ {
		if y%4 < 2 {
			for x := 20; x < 22; x++ {
				im.SetNRGBA(x, y, ink)
			}
		}
	}
	for _, line := range [][4]int{{8, 10, 16, 12}, {8, 15, 15, 17}, {8, 20, 16, 22}} {
		for y := line[1]; y < line[3]; y++ {
			for x := line[0]; x < line[2]; x++ {
				im.SetNRGBA(x, y, ink)
			}
		}
	}
	var data bytes.Buffer
	_ = png.Encode(&data, im)
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, []uint16{0, 1, 1})
	out.Write([]byte{32, 32, 0, 0})
	_ = binary.Write(&out, binary.LittleEndian, []uint16{1, 32})
	_ = binary.Write(&out, binary.LittleEndian, uint32(data.Len()))
	_ = binary.Write(&out, binary.LittleEndian, uint32(22))
	out.Write(data.Bytes())
	return out.Bytes()
}
func (r *relay) runTray() {
	systray.Run(func() {
		systray.SetIcon(ticketIcon(color.NRGBA{110, 170, 230, 255}))
		systray.SetTooltip("CPA State · 透明转发")
		open := systray.AddMenuItem("显示 / 隐藏状态窗口", "")
		systray.AddMenuItem("签发功能暂时废弃", "").Disable()
		systray.AddSeparator()
		restart := systray.AddMenuItem("完整重启中转", "退出旧进程后重新启动，短暂中断连接")
		quit := systray.AddMenuItem("退出中转", "")
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			var lastColor uint32
			for {
				select {
				case <-r.ctx.Done():
					systray.Quit()
					return
				case <-quit.ClickedCh:
					r.cancel()
					systray.Quit()
					return
				case <-open.ClickedCh:
					toggleNativeWindow()
				case <-restart.ClickedCh:
					if err := r.startRestart(); err != nil {
						r.report(err)
					}
				case <-tick.C:
					s := r.status()
					top, _, _, _, c := s.lines()
					systray.SetTooltip(fmt.Sprintf("CPA State · %d 个账号 · 未通过 %d · %s", len(s.Accounts), s.Failed, top))
					if c != lastColor {
						systray.SetIcon(ticketIcon(color.NRGBA{byte(c), byte(c >> 8), byte(c >> 16), 255}))
						lastColor = c
					}
				}
			}
		}()
	}, func() { r.cancel() })
}
