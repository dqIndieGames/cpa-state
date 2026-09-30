package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type clashFile struct {
	Proxies []map[string]any `yaml:"proxies"`
}

func (r *relay) startMihomo() error {
	if conn, err := net.DialTimeout("tcp", probeAddr, 300*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("独立代理端口 17993 已占用，未启动或接管其他进程")
	}
	home, _ := os.UserHomeDir()
	src := filepath.Join(home, "AppData", "Roaming", "io.github.clash-verge-rev.clash-verge-rev", "clash-verge.yaml")
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取 Clash Verge 配置失败: %w", err)
	}
	var cf clashFile
	if err = yaml.Unmarshal(b, &cf); err != nil {
		return fmt.Errorf("解析 Clash Verge 配置失败: %w", err)
	}
	find := func(name string) map[string]any {
		for _, n := range cf.Proxies {
			if fmt.Sprint(n["name"]) == name {
				return n
			}
		}
		return nil
	}
	baseName, exitName := os.Getenv("CPA_PROXY_BASE"), os.Getenv("CPA_PROXY_EXIT")
	if baseName == "" || exitName == "" {
		return fmt.Errorf("legacy independent proxy requires CPA_PROXY_BASE and CPA_PROXY_EXIT; ordinary forwarding does not need these")
	}
	base, isp := find(baseName), find(exitName)
	if base == nil || isp == nil {
		return fmt.Errorf("configured legacy proxy nodes are missing from Clash Verge")
	}
	base = cloneMap(base)
	isp = cloneMap(isp)
	isp["dialer-proxy"] = baseName
	data := map[string]any{"mixed-port": 17993, "bind-address": "127.0.0.1", "allow-lan": false, "mode": "rule", "log-level": "warning", "ipv6": false, "tun": map[string]any{"enable": false}, "proxies": []any{base, isp}, "rules": []string{"MATCH," + exitName}}
	if iface := os.Getenv("CPA_PROXY_INTERFACE"); iface != "" {
		data["interface-name"] = iface
	}
	root := filepath.Join(os.Getenv("LOCALAPPDATA"), "cpa-state-relay", "runtime")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	r.runtimeDir = root
	conf := filepath.Join(root, "mihomo.yaml")
	out, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	if err = os.WriteFile(conf, out, 0600); err != nil {
		return err
	}
	var exe string
	for _, p := range []string{os.Getenv("CPA_MIHOMO_EXE"), `C:\Program Files\Clash Verge\verge-mihomo.exe`} {
		if _, e := os.Stat(p); e == nil {
			exe = p
			break
		}
	}
	if exe == "" {
		return fmt.Errorf("找不到 verge-mihomo.exe")
	}
	r.mihomo = exec.Command(exe, "-d", root, "-f", conf)
	r.mihomo.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	r.mihomo.Stdout = io.Discard
	r.mihomo.Stderr = io.Discard
	if err = r.mihomo.Start(); err != nil {
		return fmt.Errorf("启动独立代理失败: %w", err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", probeAddr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.stopMihomo()
	return fmt.Errorf("独立代理未能就绪，仍使用 Clash Verge 默认线路")
}

func cloneMap(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *relay) stopMihomo() {
	if r.mihomo != nil && r.mihomo.Process != nil {
		_ = r.mihomo.Process.Kill()
		_ = r.mihomo.Wait()
		r.mihomo = nil
	}
	if r.runtimeDir != "" {
		_ = os.Remove(filepath.Join(r.runtimeDir, "mihomo.yaml"))
	}
}
