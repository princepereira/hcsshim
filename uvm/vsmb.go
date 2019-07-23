package uvm

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/logfields"
	"github.com/Microsoft/hcsshim/internal/requesttype"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
	"github.com/sirupsen/logrus"
)

// findVSMBShare finds a share by `hostPath`. If not found returns `ErrNotAttached`.
func (uvm *UtilityVM) findVSMBShare(ctx context.Context, hostPath string) (*vsmbShare, error) {
	share, ok := uvm.vsmbShares[hostPath]
	if !ok {
		return nil, ErrNotAttached
	}
	log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"host-path":     hostPath,
		"name":          share.name,
		"refCount":      share.refCount,
	}).Debug("uvm::findVSMBShare")
	return share, nil
}

func (share *vsmbShare) GuestPath() string {
	return `\\?\VMSMB\VSMB-{dcc079ae-60ba-4d07-847c-3493609c0870}\` + share.name
}

// AddVSMB adds a VSMB share to a Windows utility VM. Each VSMB share is ref-counted and
// only added if it isn't already. This is used for read-only layers, mapped directories
// to a container, and for mapped pipes.
func (uvm *UtilityVM) AddVSMB(ctx context.Context, hostPath string, guestRequest interface{}, options *hcsschema.VirtualSmbShareOptions) (err error) {
	op := "uvm::AddVSMB"
	l := log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"host-path":     hostPath,
	})
	l.WithFields(logrus.Fields{
		"options":      fmt.Sprintf("%+v", options),
		"guestRequest": fmt.Sprintf("%+v", guestRequest),
	}).Debug(op + " - Begin Operation")
	defer func() {
		if err != nil {
			l.Data[logrus.ErrorKey] = err
			l.Error(op + " - End Operation - Error")
		} else {
			l.Debug(op + " - End Operation - Success")
		}
	}()

	if uvm.operatingSystem != "windows" {
		return errNotSupported
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()
	share, err := uvm.findVSMBShare(ctx, hostPath)
	if err == ErrNotAttached {
		uvm.vsmbCounter++
		shareName := "s" + strconv.FormatUint(uvm.vsmbCounter, 16)

		modification := &hcsschema.ModifySettingRequest{
			RequestType: requesttype.Add,
			Settings: hcsschema.VirtualSmbShare{
				Name:    shareName,
				Options: options,
				Path:    hostPath,
			},
			ResourcePath: "VirtualMachine/Devices/VirtualSmb/Shares",
		}

		if err := uvm.Modify(ctx, modification); err != nil {
			return err
		}
		share = &vsmbShare{
			name:         shareName,
			guestRequest: guestRequest,
		}
		uvm.vsmbShares[hostPath] = share
	}
	share.refCount++
	return nil
}

// RemoveVSMB removes a VSMB share from a utility VM. Each VSMB share is ref-counted
// and only actually removed when the ref-count drops to zero.
func (uvm *UtilityVM) RemoveVSMB(ctx context.Context, hostPath string) (err error) {
	op := "uvm::RemoveVSMB"
	l := log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"host-path":     hostPath,
	})
	l.Debug(op + " - Begin Operation")
	defer func() {
		if err != nil {
			l.Data[logrus.ErrorKey] = err
			l.Error(op + " - End Operation - Error")
		} else {
			l.Debug(op + " - End Operation - Success")
		}
	}()

	if uvm.operatingSystem != "windows" {
		return errNotSupported
	}

	uvm.m.Lock()
	defer uvm.m.Unlock()
	share, err := uvm.findVSMBShare(ctx, hostPath)
	if err != nil {
		return fmt.Errorf("%s is not present as a VSMB share in %s, cannot remove", hostPath, uvm.id)
	}

	share.refCount--
	if share.refCount > 0 {
		return nil
	}

	modification := &hcsschema.ModifySettingRequest{
		RequestType:  requesttype.Remove,
		Settings:     hcsschema.VirtualSmbShare{Name: share.name},
		ResourcePath: "VirtualMachine/Devices/VirtualSmb/Shares",
	}
	if err := uvm.Modify(ctx, modification); err != nil {
		return fmt.Errorf("failed to remove vsmb share %s from %s: %+v: %s", hostPath, uvm.id, modification, err)
	}

	delete(uvm.vsmbShares, hostPath)
	return nil
}

// GetVSMBUvmPath returns the guest path of a VSMB mount.
func (uvm *UtilityVM) GetVSMBUvmPath(ctx context.Context, hostPath string) (_ string, err error) {
	op := "uvm::GetVSMBUvmPath"
	l := log.G(ctx).WithFields(logrus.Fields{
		logfields.UVMID: uvm.id,
		"host-path":     hostPath,
	})
	l.Debug(op + " - Begin Operation")
	defer func() {
		if err != nil {
			l.Data[logrus.ErrorKey] = err
			l.Error(op + " - End Operation - Error")
		} else {
			l.Debug(op + " - End Operation - Success")
		}
	}()

	if hostPath == "" {
		return "", fmt.Errorf("no hostPath passed to GetVSMBUvmPath")
	}
	uvm.m.Lock()
	defer uvm.m.Unlock()
	share, err := uvm.findVSMBShare(ctx, hostPath)
	if err != nil {
		return "", err
	}
	path := share.GuestPath()
	return path, nil
}
