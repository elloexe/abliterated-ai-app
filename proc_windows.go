//go:build windows

package main

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// hideWindow stops console programs (llama-server, PowerShell) from opening a
// console window, since the app itself has none.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

var (
	jobOnce   sync.Once
	jobHandle windows.Handle
)

// The job object has KILL_ON_JOB_CLOSE set: when this process exits for any
// reason, including a crash or being killed in Task Manager, Windows closes
// the handle and terminates every llama-server in the job.
func job() windows.Handle {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			logf("app", "CreateJobObject: %v", err)
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			logf("app", "SetInformationJobObject: %v", err)
		}
		jobHandle = h
	})
	return jobHandle
}

func addToJob(cmd *exec.Cmd) {
	j := job()
	if j == 0 || cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		logf("app", "OpenProcess: %v", err)
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(j, h); err != nil {
		logf("app", "AssignProcessToJobObject: %v", err)
	}
}

func messageBox(title, text string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
