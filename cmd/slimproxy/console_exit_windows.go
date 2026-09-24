package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

func standaloneConsole() bool {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) != nil ||
		windows.GetConsoleMode(windows.Handle(os.Stderr.Fd()), &mode) != nil {
		return false
	}
	if getConsoleProcessList.Find() != nil {
		return false
	}
	// A one-element buffer is sufficient: a larger return value means this
	// console is shared. Zero means failure, not an empty console.
	// https://learn.microsoft.com/en-us/windows/console/getconsoleprocesslist
	var processID uint32
	count, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&processID)), 1)
	return count == 1 && processID == uint32(os.Getpid())
}
