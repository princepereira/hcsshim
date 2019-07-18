package hcs

import (
	gcontext "context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/hcsshim/internal/cow"
	"github.com/Microsoft/hcsshim/internal/interop"
	"github.com/Microsoft/hcsshim/internal/logfields"
	"github.com/Microsoft/hcsshim/internal/schema1"
	"github.com/Microsoft/hcsshim/internal/timeout"
	"github.com/sirupsen/logrus"
)

// currentContainerStarts is used to limit the number of concurrent container
// starts.
var currentContainerStarts containerStarts

type containerStarts struct {
	maxParallel int
	inProgress  int
	sync.Mutex
}

func init() {
	mpsS := os.Getenv("HCSSHIM_MAX_PARALLEL_START")
	if len(mpsS) > 0 {
		mpsI, err := strconv.Atoi(mpsS)
		if err != nil || mpsI < 0 {
			return
		}
		currentContainerStarts.maxParallel = mpsI
	}
}

type System struct {
	handleLock     sync.RWMutex
	handle         hcsSystem
	id             string
	callbackNumber uintptr

	logctx logrus.Fields

	closedWaitOnce sync.Once
	waitBlock      chan struct{}
	waitError      error
	exitError      error

	os, typ string
}

func newSystem(id string) *System {
	return &System{
		id: id,
		logctx: logrus.Fields{
			logfields.ContainerID: id,
		},
		waitBlock: make(chan struct{}),
	}
}

func (computeSystem *System) logOperationBegin(operation string) {
	logOperationBegin(
		computeSystem.logctx,
		operation+" - Begin Operation")
}

func (computeSystem *System) logOperationEnd(operation string, err error) {
	result, err := getOperationLogResult(err)
	logOperationEnd(
		computeSystem.logctx,
		operation+" - End Operation - "+result,
		err)
}

// CreateComputeSystem creates a new compute system with the given configuration but does not start it.
func CreateComputeSystem(id string, hcsDocumentInterface interface{}) (_ *System, err error) {
	operation := "hcsshim::CreateComputeSystem"

	computeSystem := newSystem(id)
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	hcsDocumentB, err := json.Marshal(hcsDocumentInterface)
	if err != nil {
		return nil, err
	}

	hcsDocument := string(hcsDocumentB)

	logrus.WithFields(computeSystem.logctx).
		WithField(logfields.JSON, hcsDocument).
		Debug("HCS ComputeSystem Document")

	var (
		resultp     *uint16
		identity    syscall.Handle
		createError error
	)
	createError = hcsCreateComputeSystemContext(gcontext.TODO(), id, hcsDocument, identity, &computeSystem.handle, &resultp)
	if createError == nil || IsPending(createError) {
		defer func() {
			if err != nil {
				computeSystem.Close()
			}
		}()
		if err = computeSystem.registerCallback(); err != nil {
			// Terminate the compute system if it still exists. We're okay to
			// ignore a failure here.
			computeSystem.Terminate()
			return nil, makeSystemError(computeSystem, operation, "", err, nil)
		}
	}

	events, err := processAsyncHcsResult(createError, resultp, computeSystem.callbackNumber, hcsNotificationSystemCreateCompleted, &timeout.SystemCreate)
	if err != nil {
		if err == ErrTimeout {
			// Terminate the compute system if it still exists. We're okay to
			// ignore a failure here.
			computeSystem.Terminate()
		}
		return nil, makeSystemError(computeSystem, operation, hcsDocument, err, events)
	}
	go computeSystem.waitBackground()
	if err = computeSystem.getCachedProperties(); err != nil {
		return nil, err
	}
	return computeSystem, nil
}

// OpenComputeSystem opens an existing compute system by ID.
func OpenComputeSystem(id string) (_ *System, err error) {
	operation := "hcsshim::OpenComputeSystem"

	computeSystem := newSystem(id)
	computeSystem.logOperationBegin(operation)
	defer func() {
		if IsNotExist(err) {
			computeSystem.logOperationEnd(operation, nil)
		} else {
			computeSystem.logOperationEnd(operation, err)
		}
	}()

	var (
		handle  hcsSystem
		resultp *uint16
	)
	err = hcsOpenComputeSystemContext(gcontext.TODO(), id, &handle, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return nil, makeSystemError(computeSystem, operation, "", err, events)
	}
	computeSystem.handle = handle
	defer func() {
		if err != nil {
			computeSystem.Close()
		}
	}()
	if err = computeSystem.registerCallback(); err != nil {
		return nil, makeSystemError(computeSystem, operation, "", err, nil)
	}
	go computeSystem.waitBackground()
	if err = computeSystem.getCachedProperties(); err != nil {
		return nil, err
	}
	return computeSystem, nil
}

func (computeSystem *System) getCachedProperties() error {
	props, err := computeSystem.Properties()
	if err != nil {
		return err
	}
	computeSystem.typ = strings.ToLower(props.SystemType)
	computeSystem.os = strings.ToLower(props.RuntimeOSType)
	if computeSystem.os == "" && computeSystem.typ == "container" {
		// Pre-RS5 HCS did not return the OS, but it only supported containers
		// that ran Windows.
		computeSystem.os = "windows"
	}
	return nil
}

// OS returns the operating system of the compute system, "linux" or "windows".
func (computeSystem *System) OS() string {
	return computeSystem.os
}

// IsOCI returns whether processes in the compute system should be created via
// OCI.
func (computeSystem *System) IsOCI() bool {
	return computeSystem.os == "linux" && computeSystem.typ == "container"
}

// GetComputeSystems gets a list of the compute systems on the system that match the query
func GetComputeSystems(q schema1.ComputeSystemQuery) (_ []schema1.ContainerProperties, err error) {
	operation := "hcsshim::GetComputeSystems"
	fields := logrus.Fields{}
	logOperationBegin(
		fields,
		operation+" - Begin Operation")

	defer func() {
		var result string
		if err == nil {
			result = "Success"
		} else {
			result = "Error"
		}

		logOperationEnd(
			fields,
			operation+" - End Operation - "+result,
			err)
	}()

	queryb, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}

	query := string(queryb)

	logrus.WithFields(fields).
		WithField(logfields.JSON, query).
		Debug("HCS ComputeSystem Query")

	var (
		resultp         *uint16
		computeSystemsp *uint16
	)
	err = hcsEnumerateComputeSystemsContext(gcontext.TODO(), query, &computeSystemsp, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return nil, &HcsError{Op: operation, Err: err, Events: events}
	}

	if computeSystemsp == nil {
		return nil, ErrUnexpectedValue
	}
	computeSystemsRaw := interop.ConvertAndFreeCoTaskMemBytes(computeSystemsp)
	computeSystems := []schema1.ContainerProperties{}
	if err = json.Unmarshal(computeSystemsRaw, &computeSystems); err != nil {
		return nil, err
	}

	return computeSystems, nil
}

// Start synchronously starts the computeSystem.
func (computeSystem *System) Start() (err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Start"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	if computeSystem.handle == 0 {
		return makeSystemError(computeSystem, "Start", "", ErrAlreadyClosed, nil)
	}

	// This is a very simple backoff-retry loop to limit the number
	// of parallel container starts if environment variable
	// HCSSHIM_MAX_PARALLEL_START is set to a positive integer.
	// It should generally only be used as a workaround to various
	// platform issues that exist between RS1 and RS4 as of Aug 2018
	if currentContainerStarts.maxParallel > 0 {
		for {
			currentContainerStarts.Lock()
			if currentContainerStarts.inProgress < currentContainerStarts.maxParallel {
				currentContainerStarts.inProgress++
				currentContainerStarts.Unlock()
				break
			}
			if currentContainerStarts.inProgress == currentContainerStarts.maxParallel {
				currentContainerStarts.Unlock()
				time.Sleep(100 * time.Millisecond)
			}
		}
		// Make sure we decrement the count when we are done.
		defer func() {
			currentContainerStarts.Lock()
			currentContainerStarts.inProgress--
			currentContainerStarts.Unlock()
		}()
	}

	var resultp *uint16
	err = hcsStartComputeSystemContext(gcontext.TODO(), computeSystem.handle, "", &resultp)
	events, err := processAsyncHcsResult(err, resultp, computeSystem.callbackNumber, hcsNotificationSystemStartCompleted, &timeout.SystemStart)
	if err != nil {
		return makeSystemError(computeSystem, "Start", "", err, events)
	}

	return nil
}

// ID returns the compute system's identifier.
func (computeSystem *System) ID() string {
	return computeSystem.id
}

// Shutdown requests a compute system shutdown.
func (computeSystem *System) Shutdown() (err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Shutdown"
	computeSystem.logOperationBegin(operation)
	defer func() {
		computeSystem.logOperationEnd(operation, err)
	}()

	if computeSystem.handle == 0 {
		return nil
	}

	var resultp *uint16
	err = hcsShutdownComputeSystemContext(gcontext.TODO(), computeSystem.handle, "", &resultp)
	events := processHcsResult(resultp)
	switch err {
	case nil, ErrVmcomputeAlreadyStopped, ErrComputeSystemDoesNotExist, ErrVmcomputeOperationPending:
	default:
		return makeSystemError(computeSystem, "Shutdown", "", err, events)
	}
	return nil
}

// Terminate requests a compute system terminate.
func (computeSystem *System) Terminate() (err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Terminate"
	computeSystem.logOperationBegin(operation)
	defer func() {
		computeSystem.logOperationEnd(operation, err)
	}()

	if computeSystem.handle == 0 {
		return nil
	}

	var resultp *uint16
	err = hcsTerminateComputeSystemContext(gcontext.TODO(), computeSystem.handle, "", &resultp)
	events := processHcsResult(resultp)
	switch err {
	case nil, ErrVmcomputeAlreadyStopped, ErrComputeSystemDoesNotExist, ErrVmcomputeOperationPending:
	default:
		return makeSystemError(computeSystem, "Terminate", "", err, events)
	}
	return nil
}

// waitBackground waits for the compute system exit notification. Once received
// sets `computeSystem.waitError` (if any) and unblocks all `Wait` calls.
//
// This MUST be called exactly once per `computeSystem.handle` but `Wait` is
// safe to call multiple times.
func (computeSystem *System) waitBackground() {
	operation := "hcsshim::ComputeSystem::waitBackground"
	computeSystem.logOperationBegin(operation)
	err := waitForNotification(computeSystem.callbackNumber, hcsNotificationSystemExited, nil)
	switch err {
	case nil:
	case ErrVmcomputeUnexpectedExit:
		logrus.WithFields(computeSystem.logctx).Info(operation + " - unexpected system exit")
		computeSystem.exitError = makeSystemError(computeSystem, "Wait", "", err, nil)
		err = nil
	default:
		err = makeSystemError(computeSystem, "Wait", "", err, nil)
	}
	computeSystem.logOperationEnd(operation, err)
	computeSystem.closedWaitOnce.Do(func() {
		computeSystem.waitError = err
		close(computeSystem.waitBlock)
	})
}

// Wait synchronously waits for the compute system to shutdown or terminate. If
// the compute system has already exited returns the previous error (if any).
func (computeSystem *System) Wait() (err error) {
	<-computeSystem.waitBlock
	return computeSystem.waitError
}

// ExitError returns an error describing the reason the compute system terminated.
func (computeSystem *System) ExitError() (err error) {
	select {
	case <-computeSystem.waitBlock:
		if computeSystem.waitError != nil {
			return computeSystem.waitError
		}
		return computeSystem.exitError
	default:
		return errors.New("container not exited")
	}
}

func (computeSystem *System) Properties(types ...schema1.PropertyType) (_ *schema1.ContainerProperties, err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Properties"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	queryBytes, err := json.Marshal(schema1.PropertyQuery{PropertyTypes: types})
	if err != nil {
		return nil, makeSystemError(computeSystem, "Properties", "", err, nil)
	}

	queryString := string(queryBytes)
	logrus.WithFields(computeSystem.logctx).
		WithField(logfields.JSON, queryString).
		Debug("HCS ComputeSystem Properties Query")

	var resultp, propertiesp *uint16
	err = hcsGetComputeSystemPropertiesContext(gcontext.TODO(), computeSystem.handle, string(queryString), &propertiesp, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return nil, makeSystemError(computeSystem, "Properties", "", err, events)
	}

	if propertiesp == nil {
		return nil, ErrUnexpectedValue
	}
	propertiesRaw := interop.ConvertAndFreeCoTaskMemBytes(propertiesp)
	properties := &schema1.ContainerProperties{}
	if err := json.Unmarshal(propertiesRaw, properties); err != nil {
		return nil, makeSystemError(computeSystem, "Properties", "", err, nil)
	}

	return properties, nil
}

// Pause pauses the execution of the computeSystem. This feature is not enabled in TP5.
func (computeSystem *System) Pause() (err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Pause"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	if computeSystem.handle == 0 {
		return makeSystemError(computeSystem, "Pause", "", ErrAlreadyClosed, nil)
	}

	var resultp *uint16
	err = hcsPauseComputeSystemContext(gcontext.TODO(), computeSystem.handle, "", &resultp)
	events, err := processAsyncHcsResult(err, resultp, computeSystem.callbackNumber, hcsNotificationSystemPauseCompleted, &timeout.SystemPause)
	if err != nil {
		return makeSystemError(computeSystem, "Pause", "", err, events)
	}

	return nil
}

// Resume resumes the execution of the computeSystem. This feature is not enabled in TP5.
func (computeSystem *System) Resume() (err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Resume"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	if computeSystem.handle == 0 {
		return makeSystemError(computeSystem, "Resume", "", ErrAlreadyClosed, nil)
	}

	var resultp *uint16
	err = hcsResumeComputeSystemContext(gcontext.TODO(), computeSystem.handle, "", &resultp)
	events, err := processAsyncHcsResult(err, resultp, computeSystem.callbackNumber, hcsNotificationSystemResumeCompleted, &timeout.SystemResume)
	if err != nil {
		return makeSystemError(computeSystem, "Resume", "", err, events)
	}

	return nil
}

func (computeSystem *System) createProcess(c interface{}) (_ *Process, _ *hcsProcessInformation, err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::CreateProcess"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	var (
		processInfo   hcsProcessInformation
		processHandle hcsProcess
		resultp       *uint16
	)

	if computeSystem.handle == 0 {
		return nil, nil, makeSystemError(computeSystem, "CreateProcess", "", ErrAlreadyClosed, nil)
	}

	configurationb, err := json.Marshal(c)
	if err != nil {
		return nil, nil, makeSystemError(computeSystem, "CreateProcess", "", err, nil)
	}

	configuration := string(configurationb)

	logrus.WithFields(computeSystem.logctx).
		WithField(logfields.JSON, configuration).
		Debug("HCS ComputeSystem Process Document")

	err = hcsCreateProcessContext(gcontext.TODO(), computeSystem.handle, configuration, &processInfo, &processHandle, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return nil, nil, makeSystemError(computeSystem, "CreateProcess", configuration, err, events)
	}

	logrus.WithFields(computeSystem.logctx).
		WithField(logfields.ProcessID, processInfo.ProcessId).
		Debug("HCS ComputeSystem CreateProcess PID")

	return newProcess(processHandle, int(processInfo.ProcessId), computeSystem), &processInfo, nil
}

// CreateProcessNoStdio launches a new process within the computeSystem. The
// Stdio handles are not cached on the process struct.
func (computeSystem *System) CreateProcessNoStdio(c interface{}) (_ cow.Process, err error) {
	process, processInfo, err := computeSystem.createProcess(c)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			process.Close()
		}
	}()

	// We don't do anything with these handles. Close them so they don't leak.
	syscall.Close(processInfo.StdInput)
	syscall.Close(processInfo.StdOutput)
	syscall.Close(processInfo.StdError)

	if err = process.registerCallback(); err != nil {
		return nil, makeSystemError(computeSystem, "CreateProcess", "", err, nil)
	}
	go process.waitBackground()

	return process, nil
}

// CreateProcess launches a new process within the computeSystem.
func (computeSystem *System) CreateProcess(c interface{}) (_ cow.Process, err error) {
	process, processInfo, err := computeSystem.createProcess(c)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			process.Close()
		}
	}()

	pipes, err := makeOpenFiles([]syscall.Handle{processInfo.StdInput, processInfo.StdOutput, processInfo.StdError})
	if err != nil {
		return nil, makeSystemError(computeSystem, "CreateProcess", "", err, nil)
	}
	process.stdin = pipes[0]
	process.stdout = pipes[1]
	process.stderr = pipes[2]

	if err = process.registerCallback(); err != nil {
		return nil, makeSystemError(computeSystem, "CreateProcess", "", err, nil)
	}
	go process.waitBackground()

	return process, nil
}

// OpenProcess gets an interface to an existing process within the computeSystem.
func (computeSystem *System) OpenProcess(pid int) (_ *Process, err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	// Add PID for the context of this operation
	computeSystem.logctx[logfields.ProcessID] = pid
	defer delete(computeSystem.logctx, logfields.ProcessID)

	operation := "hcsshim::ComputeSystem::OpenProcess"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	var (
		processHandle hcsProcess
		resultp       *uint16
	)

	if computeSystem.handle == 0 {
		return nil, makeSystemError(computeSystem, "OpenProcess", "", ErrAlreadyClosed, nil)
	}

	err = hcsOpenProcessContext(gcontext.TODO(), computeSystem.handle, uint32(pid), &processHandle, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return nil, makeSystemError(computeSystem, "OpenProcess", "", err, events)
	}

	process := newProcess(processHandle, pid, computeSystem)
	if err = process.registerCallback(); err != nil {
		return nil, makeSystemError(computeSystem, "OpenProcess", "", err, nil)
	}
	go process.waitBackground()

	return process, nil
}

// Close cleans up any state associated with the compute system but does not terminate or wait for it.
func (computeSystem *System) Close() (err error) {
	computeSystem.handleLock.Lock()
	defer computeSystem.handleLock.Unlock()

	operation := "hcsshim::ComputeSystem::Close"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	// Don't double free this
	if computeSystem.handle == 0 {
		return nil
	}

	if err = computeSystem.unregisterCallback(); err != nil {
		return makeSystemError(computeSystem, "Close", "", err, nil)
	}

	err = hcsCloseComputeSystemContext(gcontext.TODO(), computeSystem.handle)
	if err != nil {
		return makeSystemError(computeSystem, "Close", "", err, nil)
	}

	computeSystem.handle = 0
	computeSystem.closedWaitOnce.Do(func() {
		computeSystem.waitError = ErrAlreadyClosed
		close(computeSystem.waitBlock)
	})

	return nil
}

func (computeSystem *System) registerCallback() error {
	context := &notifcationWatcherContext{
		channels: newSystemChannels(),
		systemID: computeSystem.id,
	}

	callbackMapLock.Lock()
	callbackNumber := nextCallback
	nextCallback++
	callbackMap[callbackNumber] = context
	callbackMapLock.Unlock()

	var callbackHandle hcsCallback
	err := hcsRegisterComputeSystemCallbackContext(gcontext.TODO(), computeSystem.handle, notificationWatcherCallback, callbackNumber, &callbackHandle)
	if err != nil {
		return err
	}
	context.handle = callbackHandle
	computeSystem.callbackNumber = callbackNumber

	return nil
}

func (computeSystem *System) unregisterCallback() error {
	callbackNumber := computeSystem.callbackNumber

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

	// hcsUnregisterComputeSystemCallback has its own syncronization
	// to wait for all callbacks to complete. We must NOT hold the callbackMapLock.
	err := hcsUnregisterComputeSystemCallbackContext(gcontext.TODO(), handle)
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

// Modify the System by sending a request to HCS
func (computeSystem *System) Modify(config interface{}) (err error) {
	computeSystem.handleLock.RLock()
	defer computeSystem.handleLock.RUnlock()

	operation := "hcsshim::ComputeSystem::Modify"
	computeSystem.logOperationBegin(operation)
	defer func() { computeSystem.logOperationEnd(operation, err) }()

	if computeSystem.handle == 0 {
		return makeSystemError(computeSystem, "Modify", "", ErrAlreadyClosed, nil)
	}

	requestJSON, err := json.Marshal(config)
	if err != nil {
		return err
	}

	requestString := string(requestJSON)

	logrus.WithFields(computeSystem.logctx).
		WithField(logfields.JSON, requestString).
		Debug("HCS ComputeSystem Modify Document")

	var resultp *uint16
	err = hcsModifyComputeSystemContext(gcontext.TODO(), computeSystem.handle, requestString, &resultp)
	events := processHcsResult(resultp)
	if err != nil {
		return makeSystemError(computeSystem, "Modify", requestString, err, events)
	}

	return nil
}
