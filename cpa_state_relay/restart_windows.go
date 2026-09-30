package main

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// The helper must outlive this process and complete stop/start/health/rollback.
func (r *relay) startRestart() (err error) {
	r.mu.Lock()
	if r.restartPending {
		r.mu.Unlock()
		return fmt.Errorf("完整重启已在进行中")
	}
	r.restartPending = true
	r.mu.Unlock()
	defer func() {
		if err != nil {
			r.mu.Lock()
			r.restartPending = false
			r.mu.Unlock()
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	script := filepath.Join(filepath.Dir(executable), "restart.ps1")
	if _, err = os.Stat(script); err != nil {
		return err
	}
	pwsh, err := exec.LookPath("pwsh.exe")
	if err != nil {
		return err
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	_, port, err := net.SplitHostPort(r.address)
	if err != nil {
		return err
	}
	base := filepath.Join(filepath.Dir(r.settingsPath()), "restarts")
	if err = os.MkdirAll(base, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(base, "restart-")
	if err != nil {
		return err
	}
	count := 0
	for _, account := range r.status().Accounts {
		if account.Bound {
			count++
		}
	}
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
		"-RestartOnly", "-Candidate", executable, "-Target", executable,
		"-ExpectedHash", hash, "-ExpectedCurrentHash", hash,
		"-ResultDirectory", filepath.Join(dir, "result"), "-Settings", r.settingsPath(),
		"-Port", port, "-ExpectedVersion", buildVersion, "-MinimumAccounts", strconv.Itoa(count), "-GraceSeconds", "2"}
	if r.accountHome != "" {
		args = append(args, "-AccountHome", r.accountHome)
	}
	if r.headless {
		args = append(args, "-Headless")
	}
	cmd := exec.Command(pwsh, args...)
	cmd.Dir = filepath.Dir(executable)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		r.restartPending = false
		r.mu.Unlock()
		if err != nil {
			r.report(fmt.Errorf("完整重启失败，请查看 %s: %w", filepath.Join(dir, "result", "restart-result.json"), err))
		}
	}()
	return nil
}
