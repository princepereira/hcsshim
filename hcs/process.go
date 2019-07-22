package hcs

import (
	gcontext "context"
	"encoding/json"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/hcsshim/internal/interop"
	"github.com/Microsoft/hcsshim/internal/logfields"
	"github.com/sirupsen/logrus"
)

// ContainerError is an error encountered in HCS
type Process struct {
	handleLock     sync.RWMutex
	handle         hcsProcess
	processID      int
	system         *System
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	stderr         io.ReadCloser
	callbackNumber uintptr

	logctx logrus.Fields

	closedWaitOnce sync.Once
	waitBlock      chan struct{}
	exitCode       int
	waitError      error
}

func newProcess(process hcsProcess, processID int, computeSystem *System) *Process {
	return &Process{
		handle:    process,
		processID: processID,
		system:    computeSystem,
		logctx: logrus.Fields{
			logfields.ContainerID: computeSystem.ID(),
			logfields.ProcessID:   processID,
		},
		waitBlock: make(chan struct{}),
	}
}

type processModifyRequest struct {
	Operation   string
	ConsoleSize *consoleSize `json:",omitempty"`
	CloseHandle *closeHandle `json:",omitempty"`
}

type consoleSize struct {
	Height uint16
	Width  uint16
}

type closeHandle struct {
	Handle string
}

type processStatus struct {
	ProcessID      uint32
	Exited         bool
	ExitCode       uint32
	LastWaitResult int32
}

const (
	stdIn  string = "StdIn"
	stdOut string = "StdOut"
	stdErr string = "StdErr"
)

const (
	modifyConsoleSize string = "ConsoleSize"
	modifyCloseHandle string = "CloseHandle"
)

// Pid returns the process ID of the process within the container.
func (process *Process) Pid() int {
	return process.processID
}

// SystemID returns the ID of the process's compute system.
func (process *Process) SystemID() string {
	return process.system.ID()
}

func (process *Process) logOperationBegin(operation string) {
	logOperationBegin(
		process.logctx,
		operation+" - Begin Operation")
}

func (process *Process) logOperationEnd(operation string, err error) {
	result, err := getOperationLogResult(err)
	logOperationEnd(
		process.logctx,
		operation+" - End Operation - "+result,
		err)
}

func (process *Process) processSignalResult(err error) (bool, error) {
	switch err {
	case nil:
		return true, nil
	case ErrVmcomputeOperationInvalidState, ErrComputeSystemDoesNotExist, ErrElementNotFound:
		select {
		case <-process.waitBlock:
			// The process exit notification has already arrived.
		default:
			// The process should be gone, but we have not received the notification.
			// After a second, force unblock the process wait to work around a possible
			// deadlock in the HCS.
			go func() {
				time.Sleep(time.Second)
				process.closedWaitOnce.Do(func() {
					logrus.WithFields(logrus.Fields{
						logfields.ContainerID: process.SystemID(),
						logfields.ProcessID:   process.processID,
						logrus.ErrorKey:       err,
					}).Warn("hcsshim::Process::processSignalResult - Force unblocking process waits")
					process.exitCode = -1
					process.waitError = err
					close(process.waitBlock)
				})
			}()
		}
		return false, nil
	default:
		return false, err
	}
}

// Signal signals the process with `options`.
//
// For LCOW `guestrequest.SignalProcessOptionsLCOW`.
//
// For WCOW `guestrequest.SignalProcessOptionsWCOW`.
func (process *Process) Signal(options interface{}) (_ bool, err error) {
	process.handleLock.RLock()
	defer process.handleLock.RUnlock()

	operation := "hcsshim::Process::Signal"
	process.logOperationBegin(operation)
	defer func() { process.logOperationEnd(operation, err) }()

	if process.handle == 0 {
		return false, makeProcessError(process, operation, ErrAlreadyClosed, nil)
	}

	optionsb, err := json.Marshal(options)
	if err != nil {
		return false, err
	}

	optionsStr := string(optionsb)

	var resultp *uint16
	err = hcsSignalProcessContext(gcontext.TODO(), process.handle, optionsStr, &resultp)
	events := processHcsResult(resultp)
	delivered, err := process.processSignalResult(err)
	if err != nil {
		err = makeProcessError(process, operation, err, events)
	}
	return delivered, err
}

// Kill signals the process to terminate but does not wait for it to finish terminating.
func (process *Process) Kill() (_ bool, err error) {
	process.handleLock.RLock()
	defer process.handleLock.RUnlock()

	operation := "hcsshim::Process::Kill"
	process.logOperationBegin(operation)
	defer func() { process.logOperationEnd(operation, err) }()

	if process.handle == 0 {
		return false, makeProcessError(process, operation, ErrAlreadyClosed, nil)
	}

	var resultp *uint16
	err = hcsTerminateProcessContext(gcontext.TODO(), process.handle, &resultp)
	events := processHcsResult(resultp)
	delivered, err := process.processSignalResult(err)
	if err != nil {
		err = makeProcessError(process, operation, err, events)
	}
	return delivered, err
}

// waitBackground waits for the process exit notification. Once received sets
// `process.waitError` (if any) and unblocks all `Wait` calls.
//
// This MUST be called exactly once per `process.handle` but `Wait` is safe to
// call multiple times.
func (process *Process) waitBackground() {
	operation := "hcsshim::Process::waitBackground"
	process.logOperationBegin(operation)

	var (
		err      error
		exitCode = -1
	)

	err = waitForNotification(process.callbackNumber, hcsNotificationProcessExited, nil)
	if err != nil {
		err = makeProcessError(process, operation, err, nil)
		logrus.WithFields(logrus.Fields{
			logfields.ContainerID: process.SystemID(),
			logfields.ProcessID:   process.processID,
			logrus.ErrorKey:       err,
		}).Errorf("%s - failed wait", operation)
	} else {
		process.handleLock.RLock()
		defer process.handleLock.RUnlock()

		// Make sure we didnt race with Close() here
		if process.handle != 0 {
			var (
				resultp     *uint16
				propertiesp *uint16
			)
			err = hcsGetProcessPropertiesContext(gcontext.TODO(), process.handle, &propertiesp, &resultp)
			events := processHcsResult(resultp)
			if err != nil {
				err = makeProcessError(process, operation, err, events)
			} else {
				properties := &processStatus{}
				err = json.Unmarshal(interop.ConvertAndFreeCoTaskMemBytes(propertiesp), properties)
				if err != nil {
					err = makeProcessError(process, operation, err, nil)
				} else {
					if properties.LastWaitResult != 0 {
						logrus.WithFields(logrus.Fields{
							logfields.ContainerID: process.SystemID(),
							logfields.ProcessID:   process.processID,
							"wait-result":         properties.LastWaitResult,
						}).Warningf("%s - Non-zero last wait result", operation)
					} else {
						exitCode = int(properties.ExitCode)
					}
				}
			}
		}
	}
	logrus.WithFields(logrus.Fields{
		logfields.ContainerID: process.SystemID(),
		logfields.ProcessID:   process.processID,
		"exitCode":            exitCode,
	}).Debugf("%s - process exited", operation)

	process.closedWaitOnce.Do(func() {
		process.exitCode = exitCode
		process.waitError = err
		close(process.waitBlock)
	})
	process.logOperationEnd(operation, err)
}

// Wait waits for the process to exit. If the process has already exited returns
// the pervious error (if any).
func (process *Process) Wait() (err error) {
	<-process.waitBlock
	return process.waitError
}

// ResizeConsole resizes the console of the process.
func (process *Process) ResizeConsole(width, height uint16) (err error) {
	process.handleLock.RLock()
	defer process.handleLock.RUnlock()

	operation := "hcsshim::Process::ResizeConsole"
	process.logOperationBegin(operation)
	defer func() { process.logOperationEnd(operation, err) }()

	if process.handle == 0 {
		return makeProcessError(process, operation, ErrAlreadyClosed, nil)
	}

	modifyRequest := processModifyRequest{
		Operation: modifyConsoleSize,
		ConsoleSize: &consoleSize{
			Height: height,
			Width:  width,
		},
	}

	modifyRequestb, err := json.Marshal(modifyRequest)
	if err != nil {
		return err
	}

	modifyRequestStr := string(modifyRequestb)

	var resultp *uint16
	err = hcsModifyProcessContext(gcontext.TODO(), process.handle, modifyRequestStr, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return makeProcessError(process, operation, err, events)
	}

	return nil
}

// ExitCode returns the exit code of the process. The process must have
// already terminated.
func (process *Process) ExitCode() (_ int, err error) {
	select {
	case <-process.waitBlock:
		if process.waitError != nil {
			return -1, process.waitError
		}
		return process.exitCode, nil
	default:
		return -1, makeProcessError(process, "hcsshim::Process::ExitCode", ErrInvalidProcessState, nil)
	}
}

// StdioLegacy returns the stdin, stdout, and stderr pipes, respectively. Closing
// these pipes does not close the underlying pipes; but this function can only
// be called once on each Process.
func (process *Process) StdioLegacy() (_ io.WriteCloser, _ io.ReadCloser, _ io.ReadCloser, err error) {
	process.handleLock.RLock()
	defer process.handleLock.RUnlock()

	operation := "hcsshim::Process::Stdio"
	process.logOperationBegin(operation)
	defer func() { process.logOperationEnd(operation, err) }()

	if process.handle == 0 {
		return nil, nil, nil, makeProcessError(process, operation, ErrAlreadyClosed, nil)
	}

	var (
		processInfo hcsProcessInformation
		resultp     *uint16
	)
	err = hcsGetProcessInfoContext(gcontext.TODO(), process.handle, &processInfo, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return nil, nil, nil, makeProcessError(process, operation, err, events)
	}

	pipes, err := makeOpenFiles([]syscall.Handle{processInfo.StdInput, processInfo.StdOutput, processInfo.StdError})
	if err != nil {
		return nil, nil, nil, makeProcessError(process, operation, err, nil)
	}

	return pipes[0], pipes[1], pipes[2], nil
}

// Stdio returns the stdin, stdout, and stderr pipes, respectively.
// To close them, close the process handle.
func (process *Process) Stdio() (stdin io.Writer, stdout, stderr io.Reader) {
	return process.stdin, process.stdout, process.stderr
}

// CloseStdin closes the write side of the stdin pipe so that the process is
// notified on the read side that there is no more data in stdin.
func (process *Process) CloseStdin() (err error) {
	process.handleLock.RLock()
	defer process.handleLock.RUnlock()

	operation := "hcsshim::Process::CloseStdin"
	process.logOperationBegin(operation)
	defer func() { process.logOperationEnd(operation, err) }()

	if process.handle == 0 {
		return makeProcessError(process, operation, ErrAlreadyClosed, nil)
	}

	modifyRequest := processModifyRequest{
		Operation: modifyCloseHandle,
		CloseHandle: &closeHandle{
			Handle: stdIn,
		},
	}

	modifyRequestb, err := json.Marshal(modifyRequest)
	if err != nil {
		return err
	}

	modifyRequestStr := string(modifyRequestb)

	var resultp *uint16
	err = hcsModifyProcessContext(gcontext.TODO(), process.handle, modifyRequestStr, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return makeProcessError(process, operation, err, events)
	}

	if process.stdin != nil {
		process.stdin.Close()
	}
	return nil
}

// Close cleans up any state associated with the process but does not kill
// or wait on it.
func (process *Process) Close() (err error) {
	process.handleLock.Lock()
	defer process.handleLock.Unlock()

	operation := "hcsshim::Process::Close"
	process.logOperationBegin(operation)
	defer func() { process.logOperationEnd(operation, err) }()

	// Don't double free this
	if process.handle == 0 {
		return nil
	}

	if process.stdin != nil {
		process.stdin.Close()
	}
	if process.stdout != nil {
		process.stdout.Close()
	}
	if process.stderr != nil {
		process.stderr.Close()
	}

	if err = process.unregisterCallback(); err != nil {
		return makeProcessError(process, operation, err, nil)
	}

	if err = hcsCloseProcessContext(gcontext.TODO(), process.handle); err != nil {
		return makeProcessError(process, operation, err, nil)
	}

	process.handle = 0
	process.closedWaitOnce.Do(func() {
		process.exitCode = -1
		process.waitError = ErrAlreadyClosed
		close(process.waitBlock)
	})

	return nil
}

func (process *Process) registerCallback() error {
	context := &notifcationWatcherContext{
		channels:  newProcessChannels(),
		systemID:  process.SystemID(),
		processID: process.processID,
	}

	callbackMapLock.Lock()
	callbackNumber := nextCallback
	nextCallback++
	callbackMap[callbackNumber] = context
	callbackMapLock.Unlock()

	var callbackHandle hcsCallback
	err := hcsRegisterProcessCallbackContext(gcontext.TODO(), process.handle, notificationWatcherCallback, callbackNumber, &callbackHandle)
	if err != nil {
		return err
	}
	context.handle = callbackHandle
	process.callbackNumber = callbackNumber

	return nil
}

func (process *Process) unregisterCallback() error {
	callbackNumber := process.callbackNumber

	callbackMapLock.RLock()
	context := callbackMap[callbackNumber]
	callbackMapLock.RUnlock()

	if context == nil {
		return nil
	}

	handle := context.handle

	if handle == 0 {
		return nil
	}

	// hcsUnregisterProcessCallback has its own syncronization
	// to wait for all callbacks to complete. We must NOT hold the callbackMapLock.
	err := hcsUnregisterProcessCallbackContext(gcontext.TODO(), handle)
	if err != nil {
		return err
	}

	closeChannels(context.channels)

	callbackMapLock.Lock()
	delete(callbackMap, callbackNumber)
	callbackMapLock.Unlock()

	handle = 0

	return nil
}
