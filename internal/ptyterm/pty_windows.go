//go:build windows

package ptyterm

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const (
	createUnicodeEnvironment         = 0x00000400
	extendedStartupInfoPresent       = 0x00080000
	infinite                         = 0xffffffff
	procThreadAttributePseudoConsole = 0x00020016
	startfUseStdHandles              = 0x00000100
)

var (
	kernel32                              = syscall.NewLazyDLL("kernel32.dll")
	procClosePseudoConsole                = kernel32.NewProc("ClosePseudoConsole")
	procCreatePseudoConsole               = kernel32.NewProc("CreatePseudoConsole")
	procDeleteProcThreadAttributeList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procGetExitCodeProcess                = kernel32.NewProc("GetExitCodeProcess")
	procInitializeProcThreadAttributeList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procResizePseudoConsole               = kernel32.NewProc("ResizePseudoConsole")
	procTerminateProcess                  = kernel32.NewProc("TerminateProcess")
	procUpdateProcThreadAttribute         = kernel32.NewProc("UpdateProcThreadAttribute")
	procWaitForSingleObject               = kernel32.NewProc("WaitForSingleObject")
)

type coord struct {
	X int16
	Y int16
}

type startupInfoEx struct {
	syscall.StartupInfo
	ProcThreadAttributeList unsafe.Pointer
}

type windowsPTY struct {
	mu        sync.Mutex
	inWriter  *os.File
	outReader *os.File
	hPC       syscall.Handle
}

type windowsExitError struct {
	code int
}

func (e windowsExitError) Error() string {
	return "exit status " + strconv.Itoa(e.code)
}

func (e windowsExitError) ExitCode() int {
	return e.code
}

func startPTY(cmd *exec.Cmd, size Size) (*ptyProcess, error) {
	inputRead, inputWrite, err := createPipe()
	if err != nil {
		return nil, err
	}
	outputRead, outputWrite, err := createPipe()
	if err != nil {
		_ = syscall.CloseHandle(inputRead)
		_ = syscall.CloseHandle(inputWrite)
		return nil, err
	}

	hPC, err := createPseudoConsole(size, inputRead, outputWrite)
	if err != nil {
		_ = syscall.CloseHandle(inputRead)
		_ = syscall.CloseHandle(inputWrite)
		_ = syscall.CloseHandle(outputRead)
		_ = syscall.CloseHandle(outputWrite)
		return nil, err
	}
	pty := &windowsPTY{
		inWriter:  os.NewFile(uintptr(inputWrite), "conpty-input"),
		outReader: os.NewFile(uintptr(outputRead), "conpty-output"),
		hPC:       hPC,
	}

	process, thread, err := startConPTYProcess(cmd, hPC)
	if err != nil {
		_ = syscall.CloseHandle(inputRead)
		_ = syscall.CloseHandle(outputWrite)
		_ = pty.Close()
		return nil, err
	}
	_ = syscall.CloseHandle(inputRead)
	_ = syscall.CloseHandle(outputWrite)
	return &ptyProcess{
		io: pty,
		wait: func() Result {
			result := waitWindowsProcess(process)
			pty.closePseudoConsole()
			_ = syscall.CloseHandle(thread)
			_ = syscall.CloseHandle(process)
			return result
		},
		resize: func(size Size) error {
			return resizePseudoConsole(hPC, size)
		},
		kill: func() error {
			return terminateWindowsProcess(process)
		},
	}, nil
}

func (p *windowsPTY) Read(data []byte) (int, error) {
	if p == nil || p.outReader == nil {
		return 0, os.ErrClosed
	}
	return p.outReader.Read(data)
}

func (p *windowsPTY) Write(data []byte) (int, error) {
	if p == nil || p.inWriter == nil {
		return 0, os.ErrClosed
	}
	return p.inWriter.Write(data)
}

func (p *windowsPTY) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var err error
	if p.inWriter != nil {
		if closeErr := p.inWriter.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		p.inWriter = nil
	}
	if p.outReader != nil {
		if closeErr := p.outReader.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		p.outReader = nil
	}
	if p.hPC != 0 {
		closePseudoConsole(p.hPC)
		p.hPC = 0
	}
	return err
}

func (p *windowsPTY) closePseudoConsole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hPC != 0 {
		closePseudoConsole(p.hPC)
		p.hPC = 0
	}
}

func createPipe() (syscall.Handle, syscall.Handle, error) {
	var read syscall.Handle
	var write syscall.Handle
	if err := syscall.CreatePipe(&read, &write, nil, 0); err != nil {
		return 0, 0, err
	}
	return read, write, nil
}

func createPseudoConsole(size Size, input syscall.Handle, output syscall.Handle) (syscall.Handle, error) {
	var hPC syscall.Handle
	c := coord{X: int16(size.Cols), Y: int16(size.Rows)}
	r0, _, _ := procCreatePseudoConsole.Call(
		uintptr(*(*uint32)(unsafe.Pointer(&c))),
		uintptr(input),
		uintptr(output),
		0,
		uintptr(unsafe.Pointer(&hPC)),
	)
	if int32(r0) < 0 {
		return 0, syscall.Errno(uint32(r0))
	}
	return hPC, nil
}

func resizePseudoConsole(hPC syscall.Handle, size Size) error {
	c := coord{X: int16(size.Cols), Y: int16(size.Rows)}
	r0, _, _ := procResizePseudoConsole.Call(
		uintptr(hPC),
		uintptr(*(*uint32)(unsafe.Pointer(&c))),
	)
	if int32(r0) < 0 {
		return syscall.Errno(uint32(r0))
	}
	return nil
}

func closePseudoConsole(hPC syscall.Handle) {
	procClosePseudoConsole.Call(uintptr(hPC))
}

func startConPTYProcess(cmd *exec.Cmd, hPC syscall.Handle) (syscall.Handle, syscall.Handle, error) {
	attrList, err := newProcThreadAttributeList(1)
	if err != nil {
		return 0, 0, err
	}
	defer attrList.Delete()
	if err := attrList.Update(procThreadAttributePseudoConsole, unsafe.Pointer(uintptr(hPC)), unsafe.Sizeof(hPC)); err != nil {
		return 0, 0, err
	}

	si := startupInfoEx{}
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.StartupInfo.Flags = startfUseStdHandles
	si.ProcThreadAttributeList = attrList.Pointer()
	pi := syscall.ProcessInformation{}

	app, err := executablePath(cmd)
	if err != nil {
		return 0, 0, err
	}
	appPtr, err := syscall.UTF16PtrFromString(app)
	if err != nil {
		return 0, 0, err
	}
	args := append([]string{app}, cmd.Args[1:]...)
	commandLine, err := syscall.UTF16PtrFromString(makeCommandLine(args))
	if err != nil {
		return 0, 0, err
	}
	env, err := environmentBlock(cmd.Env)
	if err != nil {
		return 0, 0, err
	}
	var envPtr *uint16
	if len(env) != 0 {
		envPtr = &env[0]
	}
	var dirPtr *uint16
	if strings.TrimSpace(cmd.Dir) != "" {
		dirPtr, err = syscall.UTF16PtrFromString(cmd.Dir)
		if err != nil {
			return 0, 0, err
		}
	}

	err = syscall.CreateProcess(
		appPtr,
		commandLine,
		nil,
		nil,
		false,
		createUnicodeEnvironment|extendedStartupInfoPresent,
		envPtr,
		dirPtr,
		&si.StartupInfo,
		&pi,
	)
	if err != nil {
		return 0, 0, err
	}
	return pi.Process, pi.Thread, nil
}

func executablePath(cmd *exec.Cmd) (string, error) {
	if cmd == nil || len(cmd.Args) == 0 || strings.TrimSpace(cmd.Args[0]) == "" {
		return "", errors.New("command is required")
	}
	path := cmd.Path
	if strings.TrimSpace(path) == "" {
		path = cmd.Args[0]
	}
	if resolved, err := exec.LookPath(path); err == nil {
		return resolved, nil
	}
	return path, nil
}

func waitWindowsProcess(process syscall.Handle) Result {
	r0, _, err := procWaitForSingleObject.Call(uintptr(process), infinite)
	if r0 == 0xffffffff {
		return Result{ExitCode: -1, Err: err}
	}
	var code uint32
	r1, _, err := procGetExitCodeProcess.Call(uintptr(process), uintptr(unsafe.Pointer(&code)))
	if r1 == 0 {
		return Result{ExitCode: -1, Err: err}
	}
	if code != 0 {
		return Result{ExitCode: int(code), Err: windowsExitError{code: int(code)}}
	}
	return Result{ExitCode: 0}
}

func terminateWindowsProcess(process syscall.Handle) error {
	r1, _, err := procTerminateProcess.Call(uintptr(process), 1)
	if r1 == 0 {
		return err
	}
	return nil
}

type procThreadAttributeList struct {
	ptr unsafe.Pointer
	buf []byte
}

func newProcThreadAttributeList(count uint32) (*procThreadAttributeList, error) {
	var size uintptr
	err := initializeProcThreadAttributeList(nil, count, 0, &size)
	if err == nil {
		return nil, errors.New("unexpected proc thread attribute list size success")
	}
	if size == 0 {
		return nil, err
	}
	buf := make([]byte, size)
	ptr := unsafe.Pointer(&buf[0])
	if err := initializeProcThreadAttributeList(ptr, count, 0, &size); err != nil {
		return nil, err
	}
	return &procThreadAttributeList{ptr: ptr, buf: buf}, nil
}

func initializeProcThreadAttributeList(attrList unsafe.Pointer, count uint32, flags uint32, size *uintptr) error {
	r1, _, err := procInitializeProcThreadAttributeList.Call(
		uintptr(attrList),
		uintptr(count),
		uintptr(flags),
		uintptr(unsafe.Pointer(size)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func (l *procThreadAttributeList) Update(attr uintptr, value unsafe.Pointer, size uintptr) error {
	r1, _, err := procUpdateProcThreadAttribute.Call(
		uintptr(l.ptr),
		0,
		attr,
		uintptr(value),
		size,
		0,
		0,
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func (l *procThreadAttributeList) Pointer() unsafe.Pointer {
	if l == nil {
		return nil
	}
	return l.ptr
}

func (l *procThreadAttributeList) Delete() {
	if l == nil || l.ptr == nil {
		return
	}
	procDeleteProcThreadAttributeList.Call(uintptr(l.ptr))
	l.ptr = nil
	l.buf = nil
}

func environmentBlock(env []string) ([]uint16, error) {
	if len(env) == 0 {
		env = os.Environ()
	}
	env = append([]string(nil), env...)
	sort.SliceStable(env, func(i, j int) bool {
		return strings.ToUpper(env[i]) < strings.ToUpper(env[j])
	})
	joined := strings.Join(env, "\x00") + "\x00\x00"
	return utf16.Encode([]rune(joined)), nil
}

func makeCommandLine(args []string) string {
	if len(args) == 0 {
		return ""
	}
	escaped := make([]string, len(args))
	for i, arg := range args {
		escaped[i] = escapeArg(arg)
	}
	return strings.Join(escaped, " ")
}

func escapeArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		switch r {
		case '\\':
			backslashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, backslashes*2+1))
			b.WriteRune(r)
			backslashes = 0
		default:
			if backslashes != 0 {
				b.WriteString(strings.Repeat(`\`, backslashes))
				backslashes = 0
			}
			b.WriteRune(r)
		}
	}
	if backslashes != 0 {
		b.WriteString(strings.Repeat(`\`, backslashes*2))
	}
	b.WriteByte('"')
	return b.String()
}
