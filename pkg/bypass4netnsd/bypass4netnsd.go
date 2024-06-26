package bypass4netnsd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/rootless-containers/bypass4netns/pkg/api"
	"github.com/rootless-containers/bypass4netns/pkg/api/com"
	"github.com/rootless-containers/bypass4netns/pkg/util"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type Driver struct {
	BypassExecutablePath string
	ComSocketPath        string
	bypass               map[string]api.BypassStatus
	lock                 sync.RWMutex
	containerInterfaces  map[string]com.ContainerInterfaces
	interfacesLock       sync.RWMutex
	HandleC2CEnable      bool
	TracerEnable         bool
	MultinodeEnable      bool
	MultinodeEtcdAddress string
	MultinodeHostAddress string
}

func NewDriver(execPath string, comSocketPath string) *Driver {
	return &Driver{
		BypassExecutablePath: execPath,
		ComSocketPath:        comSocketPath,
		bypass:               map[string]api.BypassStatus{},
		lock:                 sync.RWMutex{},
		containerInterfaces:  map[string]com.ContainerInterfaces{},
		interfacesLock:       sync.RWMutex{},
		TracerEnable:         false,
		MultinodeEnable:      false,
	}
}

func (d *Driver) ListBypass() []api.BypassStatus {
	d.lock.RLock()
	defer d.lock.RUnlock()

	res := []api.BypassStatus{}
	for _, v := range d.bypass {
		res = append(res, v)
	}

	return res
}

func (d *Driver) StartBypass(spec *api.BypassSpec) (*api.BypassStatus, error) {
	logger := logrus.WithFields(logrus.Fields{"ID": util.ShrinkID(spec.ID)})
	logger.Info("Starting bypass")
	b4nnArgs := []string{}

	if logger.Logger.GetLevel() == logrus.DebugLevel {
		b4nnArgs = append(b4nnArgs, "--debug")
	}

	if spec.SocketPath != "" {
		socketOption := fmt.Sprintf("--socket=%s", spec.SocketPath)
		b4nnArgs = append(b4nnArgs, socketOption)
	}

	if spec.PidFilePath != "" {
		pidFileOption := fmt.Sprintf("--pid-file=%s", spec.PidFilePath)
		b4nnArgs = append(b4nnArgs, pidFileOption)
	}

	if spec.LogFilePath != "" {
		logFileOption := fmt.Sprintf("--log-file=%s", spec.LogFilePath)
		b4nnArgs = append(b4nnArgs, logFileOption)
	}

	for _, port := range spec.PortMapping {
		b4nnArgs = append(b4nnArgs, fmt.Sprintf("-p=%d:%d", port.ParentPort, port.ChildPort))
	}

	for _, subnet := range spec.IgnoreSubnets {
		b4nnArgs = append(b4nnArgs, fmt.Sprintf("--ignore=%s", subnet))
	}

	if spec.IgnoreBind {
		b4nnArgs = append(b4nnArgs, "--ignore-bind")
	}

	b4nnArgs = append(b4nnArgs, fmt.Sprintf("--com-socket=%s", d.ComSocketPath))
	if d.HandleC2CEnable {
		b4nnArgs = append(b4nnArgs, "--handle-c2c-connections")
	}
	if d.TracerEnable {
		b4nnArgs = append(b4nnArgs, "--tracer=true")
	}

	if d.MultinodeEnable {
		b4nnArgs = append(b4nnArgs, "--multinode=true")
		b4nnArgs = append(b4nnArgs, fmt.Sprintf("--multinode-etcd-address=%s", d.MultinodeEtcdAddress))
		b4nnArgs = append(b4nnArgs, fmt.Sprintf("--multinode-host-address=%s", d.MultinodeHostAddress))
	}

	// prepare pipe for ready notification
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer readyR.Close()
	defer readyW.Close()

	// fd in b4nnCmd.ExtraFiles is assigned in child process from fd=3
	readyFdOption := "--ready-fd=3"
	b4nnArgs = append(b4nnArgs, readyFdOption)

	logger.Infof("bypass4netns args:%v", b4nnArgs)
	b4nnCmd := exec.Command(d.BypassExecutablePath, b4nnArgs...)
	b4nnCmd.ExtraFiles = append(b4nnCmd.ExtraFiles, readyW)
	err = b4nnCmd.Start()
	if err != nil {
		return nil, err
	}

	err = waitForReadyFD(b4nnCmd.Process.Pid, readyR)
	if err != nil {
		return nil, err
	}
	logger.Info("bypass4netns successfully started")

	d.lock.Lock()
	defer d.lock.Unlock()
	status := api.BypassStatus{
		ID:   spec.ID,
		Pid:  b4nnCmd.Process.Pid,
		Spec: *spec,
	}

	d.bypass[status.ID] = status
	logger.Info("Started bypass")

	return &status, nil
}

func (d *Driver) StopBypass(id string) error {
	logger := logrus.WithFields(logrus.Fields{"ID": util.ShrinkID(id)})
	logger.Infof("Stopping bypass")
	d.lock.Lock()
	defer d.lock.Unlock()

	bStatus, ok := d.bypass[id]
	if !ok {
		return fmt.Errorf("child %s not found", id)
	}

	proc, err := os.FindProcess(bStatus.Pid)
	if err != nil {
		return err
	}
	logger.Debugf("bypass4netns found pid=%d", proc.Pid)

	logger.Infof("Terminating bypass4netns pid=%d", proc.Pid)
	err = proc.Signal(unix.SIGTERM)
	if err != nil {
		return err
	}

	// wait for the process exit
	// TODO: Timeout
	if _, err := proc.Wait(); err != nil {
		logrus.Warnf("Failed to terminate bypass4netns pid=%d with SIGTERM, killing...", proc.Pid)
		err = proc.Kill()
		if err != nil {
			return err
		}
		_, _ = proc.Wait()
	}
	logger.Infof("Terminated bypass4netns pid=%d", proc.Pid)

	delete(d.bypass, id)
	logger.Info("Stopped bypass")

	// remove the container's interfaces
	d.DeleteInterface(id)

	return nil
}

func (d *Driver) ListInterfaces() map[string]com.ContainerInterfaces {
	d.interfacesLock.RLock()
	defer d.interfacesLock.RUnlock()

	ifs := map[string]com.ContainerInterfaces{}
	// copy map
	for k := range d.containerInterfaces {
		ifs[k] = d.containerInterfaces[k]
	}

	return ifs
}

func (d *Driver) GetInterface(id string) *com.ContainerInterfaces {
	d.interfacesLock.RLock()
	defer d.interfacesLock.RUnlock()

	ifs, ok := d.containerInterfaces[id]
	if !ok {
		return nil
	}

	return &ifs
}

func (d *Driver) PostInterface(id string, containerIfs *com.ContainerInterfaces) {
	d.interfacesLock.Lock()
	defer d.interfacesLock.Unlock()

	d.containerInterfaces[id] = *containerIfs
}

func (d *Driver) DeleteInterface(id string) {
	d.interfacesLock.Lock()
	defer d.interfacesLock.Unlock()

	delete(d.containerInterfaces, id)
}

// waitForReady is from libpod
// https://github.com/containers/libpod/blob/e6b843312b93ddaf99d0ef94a7e60ff66bc0eac8/libpod/networking_linux.go#L272-L308
func waitForReadyFD(cmdPid int, r *os.File) error {
	b := make([]byte, 16)
	for {
		if err := r.SetDeadline(time.Now().Add(1 * time.Second)); err != nil {
			return fmt.Errorf("error setting bypass4netns pipe timeout: %w", err)
		}
		if _, err := r.Read(b); err == nil {
			break
		} else {
			if os.IsTimeout(err) {
				// Check if the process is still running.
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(cmdPid, &status, syscall.WNOHANG, nil)
				if err != nil {
					return fmt.Errorf("failed to read bypass4netns process status: %w", err)
				}
				if pid != cmdPid {
					continue
				}
				if status.Exited() {
					return errors.New("bypass4netns failed")
				}
				if status.Signaled() {
					return errors.New("bypass4netns killed by signal")
				}
				continue
			}
			return fmt.Errorf("failed to read from bypass4netns sync pipe: %w", err)
		}
	}
	return nil
}
