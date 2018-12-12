package uvm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Microsoft/hcsshim/internal/guid"
	"github.com/Microsoft/hcsshim/internal/hcs"
	"github.com/Microsoft/hcsshim/internal/mergemaps"
	"github.com/Microsoft/hcsshim/internal/schema2"
	"github.com/Microsoft/hcsshim/internal/schemaversion"
	"github.com/Microsoft/hcsshim/internal/uvmfolder"
	"github.com/Microsoft/hcsshim/internal/wcow"
	"github.com/sirupsen/logrus"
)

// OptionsWCOW are the set of options passed to CreateWCOW() to create a utility vm.
type OptionsWCOW struct {
	*Options

	LayerFolders []string // Set of folders for base layers and scratch. Ordered from top most read-only through base read-only layer, followed by scratch
}

// CreateWCOW creates an HCS compute system representing a utility VM.
//
// WCOW Notes:
//   - The scratch is always attached to SCSI 0:0
//
func CreateWCOW(opts *OptionsWCOW) (_ *UtilityVM, err error) {
	logrus.Debugf("uvm::CreateWCOW %+v", opts)

	if opts.Options == nil {
		opts.Options = &Options{}
	}

	uvm := &UtilityVM{
		id:                  opts.ID,
		owner:               opts.Owner,
		operatingSystem:     "windows",
		scsiControllerCount: 1,
		vsmbShares:          make(map[string]*vsmbShare),
	}

	// Defaults if omitted by caller.
	// TODO: Change this. Don't auto generate ID if omitted. Avoids the chicken-and-egg problem
	if uvm.id == "" {
		uvm.id = guid.New().String()
	}
	if uvm.owner == "" {
		uvm.owner = filepath.Base(os.Args[0])
	}
	if opts.UseGuestConnection == nil {
		val := true
		opts.UseGuestConnection = &val
	}

	if len(opts.LayerFolders) < 2 {
		return nil, fmt.Errorf("at least 2 LayerFolders must be supplied")
	}
	uvmFolder, err := uvmfolder.LocateUVMFolder(opts.LayerFolders)
	if err != nil {
		return nil, fmt.Errorf("failed to locate utility VM folder from layer folders: %s", err)
	}

	// TODO: BUGBUG Remove this. @jhowardmsft
	//       It should be the responsiblity of the caller to do the creation and population.
	//       - Update runhcs too (vm.go).
	//       - Remove comment in function header
	//       - Update tests that rely on this current behaviour.
	// Create the RW scratch in the top-most layer folder, creating the folder if it doesn't already exist.
	scratchFolder := opts.LayerFolders[len(opts.LayerFolders)-1]
	logrus.Debugf("uvm::CreateWCOW scratch folder: %s", scratchFolder)

	// Create the directory if it doesn't exist
	if _, err := os.Stat(scratchFolder); os.IsNotExist(err) {
		logrus.Debugf("uvm::CreateWCOW creating folder: %s ", scratchFolder)
		if err := os.MkdirAll(scratchFolder, 0777); err != nil {
			return nil, fmt.Errorf("failed to create utility VM scratch folder: %s", err)
		}
	}

	// Create sandbox.vhdx in the scratch folder based on the template, granting the correct permissions to it
	scratchPath := filepath.Join(scratchFolder, "sandbox.vhdx")
	if _, err := os.Stat(scratchPath); os.IsNotExist(err) {
		if err := wcow.CreateUVMScratch(uvmFolder, scratchFolder, uvm.id); err != nil {
			return nil, fmt.Errorf("failed to create scratch: %s", err)
		}
	}

	doc := &hcsschema.ComputeSystem{
		Owner:                             uvm.owner,
		SchemaVersion:                     schemaversion.SchemaV21(),
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: &hcsschema.VirtualMachine{
			Chipset: &hcsschema.Chipset{
				Uefi: &hcsschema.Uefi{
					BootThis: &hcsschema.UefiBootEntry{
						DevicePath: `\EFI\Microsoft\Boot\bootmgfw.efi`,
						DeviceType: "VmbFs",
					},
				},
			},
			ComputeTopology: &hcsschema.Topology{
				Memory: &hcsschema.Memory2{
					SizeInMB: getMemory(opts.Resources),
					// AllowOvercommit `true` by default if not passed.
					AllowOvercommit: opts.AllowOvercommit == nil || *opts.AllowOvercommit,
					// EnableHotHint is not compatible with physical. Only virtual, and only Windows.
					EnableHotHint: opts.AllowOvercommit == nil || *opts.AllowOvercommit,
					// EnableDeferredCommit `false` by default if not passed.
					EnableDeferredCommit: opts.EnableDeferredCommit != nil && *opts.EnableDeferredCommit,
				},
				Processor: &hcsschema.Processor2{
					Count: getProcessors(opts.Resources),
				},
			},
			Devices: &hcsschema.Devices{
				Scsi: map[string]hcsschema.Scsi{
					"0": {
						Attachments: map[string]hcsschema.Attachment{
							"0": {
								Path:  scratchPath,
								Type_: "VirtualDisk",
							},
						},
					},
				},
				HvSocket: &hcsschema.HvSocket2{
					HvSocketConfig: &hcsschema.HvSocketSystemConfig{
						// Allow administrators and SYSTEM to bind to vsock sockets
						// so that we can create a GCS log socket.
						DefaultBindSecurityDescriptor: "D:P(A;;FA;;;SY)(A;;FA;;;BA)",
					},
				},
				VirtualSmb: &hcsschema.VirtualSmb{
					DirectFileMappingInMB: 1024, // Sensible default, but could be a tuning parameter somewhere
					Shares: []hcsschema.VirtualSmbShare{
						{
							Name: "os",
							Path: filepath.Join(uvmFolder, `UtilityVM\Files`),
							Options: &hcsschema.VirtualSmbShareOptions{
								ReadOnly:            true,
								PseudoOplocks:       true,
								TakeBackupPrivilege: true,
								CacheIo:             true,
								ShareRead:           true,
							},
						},
					},
				},
			},
		},
	}

	if *opts.UseGuestConnection {
		doc.VirtualMachine.GuestConnection = &hcsschema.GuestConnection{}
	}

	uvm.scsiLocations[0][0].hostPath = doc.VirtualMachine.Devices.Scsi["0"].Attachments["0"].Path

	fullDoc, err := mergemaps.MergeJSON(doc, ([]byte)(opts.AdditionHCSDocumentJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to merge additional JSON '%s': %s", opts.AdditionHCSDocumentJSON, err)
	}

	hcsSystem, err := hcs.CreateComputeSystem(uvm.id, fullDoc)
	if err != nil {
		logrus.Debugln("failed to create UVM: ", err)
		return nil, err
	}
	uvm.hcsSystem = hcsSystem
	return uvm, nil
}
