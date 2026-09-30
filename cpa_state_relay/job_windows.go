package main

import (
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

func runInJob(cmd *exec.Cmd) error {
	job, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		return fmt.Errorf("创建CLI进程容器失败")
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); e != nil {
		return e
	}
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error {
		_ = windows.TerminateJobObject(job, 1)
		return cmd.Process.Kill()
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Attach the job before the CLI can spawn any helper process.
	cmd.SysProcAttr.CreationFlags |= 0x4
	if e = cmd.Start(); e != nil {
		return fmt.Errorf("启动CLI失败: %w", e)
	}
	handle, e := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|0x800, false, uint32(cmd.Process.Pid))
	if e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return e
	}
	e = windows.AssignProcessToJobObject(job, handle)
	if e == nil {
		status, _, _ := syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess").Call(uintptr(handle))
		if status != 0 {
			e = fmt.Errorf("恢复CLI进程失败: %x", status)
		}
	}
	_ = windows.CloseHandle(handle)
	if e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return e
	}
	if e = cmd.Wait(); e != nil {
		return fmt.Errorf("CLI未完成取票（进程已退出）")
	}
	return nil
}
