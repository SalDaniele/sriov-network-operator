package utils

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"encoding/hex"
	"hash/fnv"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/cenkalti/backoff"
	"github.com/golang/glog"
	"github.com/jaypipes/ghw"
	"github.com/vishvananda/netlink"
	"k8s.io/apimachinery/pkg/util/wait"

	dputils "github.com/k8snetworkplumbingwg/sriov-network-device-plugin/pkg/utils"

	sriovnetworkv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	constants "github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/consts"
)

const (
	sysBusPciDevices      = "/sys/bus/pci/devices"
	sysBusPciDrivers      = "/sys/bus/pci/drivers"
	sysBusPciDriversProbe = "/sys/bus/pci/drivers_probe"
	sysClassNet           = "/sys/class/net"
	procKernelCmdLine     = "/proc/cmdline"
	netClass              = 0x02
	numVfsFile            = "sriov_numvfs"

	ClusterTypeOpenshift  = "openshift"
	ClusterTypeKubernetes = "kubernetes"
	VendorMellanox        = "15b3"
	DeviceBF2             = "a2d6"
	DeviceBF3             = "a2dc"

	udevFolder      = "/etc/udev"
	udevRulesFolder = udevFolder + "/rules.d"
	udevDisableNM   = "/bindata/scripts/udev-find-sriov-pf.sh"
	nmUdevRule      = "SUBSYSTEM==\"net\", ACTION==\"add|change|move\", ATTRS{device}==\"%s\", IMPORT{program}=\"/etc/udev/disable-nm-sriov.sh $env{INTERFACE} %s\""

	KernelArgPciRealloc = "pci=realloc"
	KernelArgIntelIommu = "intel_iommu=on"
	KernelArgIommuPt    = "iommu=pt"
)

var InitialState sriovnetworkv1.SriovNetworkNodeState
var ClusterType string

var pfPhysPortNameRe = regexp.MustCompile(`p\d+`)

// FilesystemRoot used by test to mock interactions with filesystem
var FilesystemRoot = ""

var SupportedVfIds []string

func init() {
	ClusterType = os.Getenv("CLUSTER_TYPE")
}

// GetCurrentKernelArgs This retrieves the kernel cmd line arguments
func GetCurrentKernelArgs(chroot bool) (string, error) {
	path := procKernelCmdLine
	if !chroot {
		path = "/host" + path
	}
	cmdLine, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("GetCurrentKernelArgs(): Error reading %s: %v", procKernelCmdLine, err)
	}
	return string(cmdLine), nil
}

// IsKernelArgsSet This checks if the kernel cmd line is set properly. Please note that the same key could be repeated
// several times in the kernel cmd line. We can only ensure that the kernel cmd line has the key/val kernel arg that we set.
func IsKernelArgsSet(cmdLine string, karg string) bool {
	elements := strings.Fields(cmdLine)
	for _, element := range elements {
		if element == karg {
			return true
		}
	}
	return false
}

func DiscoverSriovDevices(withUnsupported bool, storeManager StoreManagerInterface) ([]sriovnetworkv1.InterfaceExt, error) {
	glog.V(2).Info("DiscoverSriovDevices")
	pfList := []sriovnetworkv1.InterfaceExt{}

	pci, err := ghw.PCI()
	if err != nil {
		return nil, fmt.Errorf("DiscoverSriovDevices(): error getting PCI info: %v", err)
	}

	devices := pci.ListDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("DiscoverSriovDevices(): could not retrieve PCI devices")
	}

	for _, device := range devices {
		devClass, err := strconv.ParseInt(device.Class.ID, 16, 64)
		if err != nil {
			glog.Warningf("DiscoverSriovDevices(): unable to parse device class for device %+v %q", device, err)
			continue
		}
		if devClass != netClass {
			// Not network device
			continue
		}

		// TODO: exclude devices used by host system

		if dputils.IsSriovVF(device.Address) {
			continue
		}

		driver, err := dputils.GetDriverName(device.Address)
		if err != nil {
			glog.Warningf("DiscoverSriovDevices(): unable to parse device driver for device %+v %q", device, err)
			continue
		}

		deviceNames, err := dputils.GetNetNames(device.Address)
		if err != nil {
			glog.Warningf("DiscoverSriovDevices(): unable to get device names for device %+v %q", device, err)
			continue
		}

		if len(deviceNames) == 0 {
			// no network devices found, skipping device
			continue
		}

		if !withUnsupported {
			if !sriovnetworkv1.IsSupportedModel(device.Vendor.ID, device.Product.ID) {
				glog.Infof("DiscoverSriovDevices(): unsupported device %+v", device)
				continue
			}
		}

		iface := sriovnetworkv1.InterfaceExt{
			PciAddress: device.Address,
			Driver:     driver,
			Vendor:     device.Vendor.ID,
			DeviceID:   device.Product.ID,
		}
		if mtu := getNetdevMTU(device.Address); mtu > 0 {
			iface.Mtu = mtu
		}
		if name := tryGetInterfaceName(device.Address); name != "" {
			iface.Name = name
			iface.Mac = getNetDevMac(name)
			iface.LinkSpeed = getNetDevLinkSpeed(name)
		}
		iface.LinkType = getLinkType(iface)

		pfStatus, exist, err := storeManager.LoadPfsStatus(iface.PciAddress)
		if err != nil {
			glog.Warningf("DiscoverSriovDevices(): failed to load PF status from disk: %v", err)
		} else {
			if exist {
				iface.ExternallyManaged = pfStatus.ExternallyManaged
			}
		}

		if dputils.IsSriovPF(device.Address) {
			iface.TotalVfs = dputils.GetSriovVFcapacity(device.Address)
			iface.NumVfs = dputils.GetVFconfigured(device.Address)
			if iface.EswitchMode, err = GetNicSriovMode(device.Address); err != nil {
				glog.Warningf("DiscoverSriovDevices(): unable to get device mode %+v %q", device.Address, err)
			}
			if dputils.SriovConfigured(device.Address) {
				vfs, err := dputils.GetVFList(device.Address)
				if err != nil {
					glog.Warningf("DiscoverSriovDevices(): unable to parse VFs for device %+v %q", device, err)
					continue
				}
				for _, vf := range vfs {
					instance := getVfInfo(vf, devices)
					iface.VFs = append(iface.VFs, instance)
				}
			}
		}
		pfList = append(pfList, iface)
	}

	return pfList, nil
}

// SyncNodeState Attempt to update the node state to match the desired state
func SyncNodeState(newState *sriovnetworkv1.SriovNetworkNodeState, pfsToConfig map[string]bool) error {
	return ConfigSriovInterfaces(newState.Spec.Interfaces, newState.Status.Interfaces, pfsToConfig)
}

func ConfigSriovInterfaces(interfaces []sriovnetworkv1.Interface, ifaceStatuses []sriovnetworkv1.InterfaceExt, pfsToConfig map[string]bool) error {
	if IsKernelLockdownMode(true) && hasMellanoxInterfacesInSpec(ifaceStatuses, interfaces) {
		glog.Warningf("cannot use mellanox devices when in kernel lockdown mode")
		return fmt.Errorf("cannot use mellanox devices when in kernel lockdown mode")
	}

	// we are already inside chroot, so we initialize the store as running on host
	storeManager, err := NewStoreManager(true)
	if err != nil {
		return fmt.Errorf("SyncNodeState(): error initializing storeManager: %v", err)
	}

	for _, ifaceStatus := range ifaceStatuses {
		configured := false
		for _, iface := range interfaces {
			if iface.PciAddress == ifaceStatus.PciAddress {
				configured = true

				if skip := pfsToConfig[iface.PciAddress]; skip {
					break
				}

				if !NeedUpdate(&iface, &ifaceStatus) {
					glog.V(2).Infof("syncNodeState(): no need update interface %s", iface.PciAddress)

					// Save the PF status to the host
					err = storeManager.SaveLastPfAppliedStatus(&iface)
					if err != nil {
						glog.Errorf("SyncNodeState(): failed to save PF applied config to host: %v", err)
						return err
					}

					break
				}
				if err = configSriovDevice(&iface, &ifaceStatus); err != nil {
					glog.Errorf("SyncNodeState(): fail to configure sriov interface %s: %v. resetting interface.", iface.PciAddress, err)
					if iface.ExternallyManaged {
						glog.Infof("SyncNodeState(): skipping device reset as the nic is marked as externally created")
					} else {
						if resetErr := resetSriovDevice(ifaceStatus); resetErr != nil {
							glog.Errorf("SyncNodeState(): failed to reset on error SR-IOV interface: %s", resetErr)
						}
					}
					return err
				}

				// Save the PF status to the host
				err = storeManager.SaveLastPfAppliedStatus(&iface)
				if err != nil {
					glog.Errorf("SyncNodeState(): failed to save PF applied config to host: %v", err)
					return err
				}
				break
			}
		}
		if !configured && ifaceStatus.NumVfs > 0 {
			if skip := pfsToConfig[ifaceStatus.PciAddress]; skip {
				continue
			}

			// load the PF info
			pfStatus, exist, err := storeManager.LoadPfsStatus(ifaceStatus.PciAddress)
			if err != nil {
				glog.Errorf("SyncNodeState(): failed to load info about PF status for pci address %s: %v", ifaceStatus.PciAddress, err)
				return err
			}

			if !exist {
				glog.Infof("SyncNodeState(): PF name %s with pci address %s has VFs configured but they weren't created by the sriov operator. Skipping the device reset",
					ifaceStatus.Name,
					ifaceStatus.PciAddress)
				continue
			}

			if pfStatus.ExternallyManaged {
				glog.Infof("SyncNodeState(): PF name %s with pci address %s was externally created skipping the device reset",
					ifaceStatus.Name,
					ifaceStatus.PciAddress)
				continue
			} else {
				err = RemoveUdevRule(ifaceStatus.PciAddress)
				if err != nil {
					return err
				}
			}

			if err = resetSriovDevice(ifaceStatus); err != nil {
				return err
			}
		}
	}
	return nil
}

// skipConfigVf Use systemd service to configure switchdev mode or BF-2 NICs in OpenShift
func skipConfigVf(ifSpec sriovnetworkv1.Interface, ifStatus sriovnetworkv1.InterfaceExt) (bool, error) {
	if ifSpec.EswitchMode == sriovnetworkv1.ESwithModeSwitchDev {
		glog.V(2).Infof("skipConfigVf(): skip config VF for switchdev device")
		return true, nil
	}

	//  NVIDIA BlueField 2 and BlueField3 in OpenShift
	if ClusterType == ClusterTypeOpenshift && ifStatus.Vendor == VendorMellanox && (ifStatus.DeviceID == DeviceBF2 || ifStatus.DeviceID == DeviceBF3) {
		// TODO: remove this when switch to the systemd configuration support.
		mode, err := mellanoxBlueFieldMode(ifStatus.PciAddress)
		if err != nil {
			return false, fmt.Errorf("failed to read Mellanox Bluefield card mode for %s,%v", ifStatus.PciAddress, err)
		}

		if mode == bluefieldConnectXMode {
			return false, nil
		}

		glog.V(2).Infof("skipConfigVf(): skip config VF for Bluefiled card on DPU mode")
		return true, nil
	}

	return false, nil
}

// GetPfsToSkip return a map of devices pci addresses to should be configured via systemd instead if the legacy mode
// we skip devices in switchdev mode and Bluefield card in ConnectX mode
func GetPfsToSkip(ns *sriovnetworkv1.SriovNetworkNodeState) (map[string]bool, error) {
	pfsToSkip := map[string]bool{}
	for _, ifaceStatus := range ns.Status.Interfaces {
		for _, iface := range ns.Spec.Interfaces {
			if iface.PciAddress == ifaceStatus.PciAddress {
				skip, err := skipConfigVf(iface, ifaceStatus)
				if err != nil {
					glog.Errorf("GetPfsToSkip(): fail to check for skip VFs %s: %v.", iface.PciAddress, err)
					return pfsToSkip, err
				}
				pfsToSkip[iface.PciAddress] = skip
				break
			}
		}
	}

	return pfsToSkip, nil
}

func NeedUpdate(iface *sriovnetworkv1.Interface, ifaceStatus *sriovnetworkv1.InterfaceExt) bool {
	if iface.Mtu > 0 {
		mtu := iface.Mtu
		if mtu != ifaceStatus.Mtu {
			glog.V(2).Infof("NeedUpdate(): MTU needs update, desired=%d, current=%d", mtu, ifaceStatus.Mtu)
			return true
		}
	}

	if iface.NumVfs != ifaceStatus.NumVfs {
		glog.V(2).Infof("NeedUpdate(): NumVfs needs update desired=%d, current=%d", iface.NumVfs, ifaceStatus.NumVfs)
		return true
	}
	if iface.NumVfs > 0 {
		for _, vf := range ifaceStatus.VFs {
			ingroup := false
			for _, group := range iface.VfGroups {
				if sriovnetworkv1.IndexInRange(vf.VfID, group.VfRange) {
					ingroup = true
					if group.DeviceType != constants.DeviceTypeNetDevice {
						if group.DeviceType != vf.Driver {
							glog.V(2).Infof("NeedUpdate(): Driver needs update, desired=%s, current=%s", group.DeviceType, vf.Driver)
							return true
						}
					} else {
						if sriovnetworkv1.StringInArray(vf.Driver, DpdkDrivers) {
							glog.V(2).Infof("NeedUpdate(): Driver needs update, desired=%s, current=%s", group.DeviceType, vf.Driver)
							return true
						}
						if vf.Mtu != 0 && group.Mtu != 0 && vf.Mtu != group.Mtu {
							glog.V(2).Infof("NeedUpdate(): VF %d MTU needs update, desired=%d, current=%d", vf.VfID, group.Mtu, vf.Mtu)
							return true
						}

						// this is needed to be sure the admin mac address is configured as expected
						if iface.ExternallyManaged {
							glog.V(2).Infof("NeedUpdate(): need to update the device as it's externally manage for pci address %s", ifaceStatus.PciAddress)
							return true
						}
					}
					break
				}
			}
			if !ingroup && sriovnetworkv1.StringInArray(vf.Driver, DpdkDrivers) {
				// VF which has DPDK driver loaded but not in any group, needs to be reset to default driver.
				return true
			}
		}
	}
	return false
}

func configSriovDevice(iface *sriovnetworkv1.Interface, ifaceStatus *sriovnetworkv1.InterfaceExt) error {
	glog.V(2).Infof("configSriovDevice(): config interface %s with %v", iface.PciAddress, iface)
	var err error
	if iface.NumVfs > ifaceStatus.TotalVfs {
		err := fmt.Errorf("cannot config SRIOV device: NumVfs (%d) is larger than TotalVfs (%d)", iface.NumVfs, ifaceStatus.TotalVfs)
		glog.Errorf("configSriovDevice(): fail to set NumVfs for device %s: %v", iface.PciAddress, err)
		return err
	}
	// set numVFs
	if iface.NumVfs != ifaceStatus.NumVfs {
		if iface.ExternallyManaged {
			if iface.NumVfs > ifaceStatus.NumVfs {
				errMsg := fmt.Sprintf("configSriovDevice(): number of request virtual functions %d is not equal to configured virtual functions %d but the policy is configured as ExternallyManaged for device %s", iface.NumVfs, ifaceStatus.NumVfs, iface.PciAddress)
				glog.Error(errMsg)
				return fmt.Errorf(errMsg)
			}
		} else {
			// create the udev rule to disable all the vfs from network manager as this vfs are managed by the operator
			err = AddUdevRule(iface.PciAddress)
			if err != nil {
				return err
			}

			err = setSriovNumVfs(iface.PciAddress, iface.NumVfs)
			if err != nil {
				err = RemoveUdevRule(iface.PciAddress)
				if err != nil {
					return err
				}
				glog.Errorf("configSriovDevice(): fail to set NumVfs for device %s", iface.PciAddress)
				return err
			}
		}
	}
	// set PF mtu
	if iface.Mtu > 0 && iface.Mtu > ifaceStatus.Mtu {
		err = setNetdevMTU(iface.PciAddress, iface.Mtu)
		if err != nil {
			glog.Warningf("configSriovDevice(): fail to set mtu for PF %s: %v", iface.PciAddress, err)
			return err
		}
	}
	// Config VFs
	if iface.NumVfs > 0 {
		vfAddrs, err := dputils.GetVFList(iface.PciAddress)
		if err != nil {
			glog.Warningf("configSriovDevice(): unable to parse VFs for device %+v %q", iface.PciAddress, err)
		}
		pfLink, err := netlink.LinkByName(iface.Name)
		if err != nil {
			glog.Errorf("configSriovDevice(): unable to get PF link for device %+v %q", iface, err)
			return err
		}

		for _, addr := range vfAddrs {
			var group sriovnetworkv1.VfGroup
			i := 0
			var dpdkDriver string
			var isRdma bool
			vfID, err := dputils.GetVFID(addr)
			for i, group = range iface.VfGroups {
				if err != nil {
					glog.Warningf("configSriovDevice(): unable to get VF id %+v %q", iface.PciAddress, err)
				}
				if sriovnetworkv1.IndexInRange(vfID, group.VfRange) {
					isRdma = group.IsRdma
					if sriovnetworkv1.StringInArray(group.DeviceType, DpdkDrivers) {
						dpdkDriver = group.DeviceType
					}
					break
				}
			}

			// only set GUID and MAC for VF with default driver
			// for userspace drivers like vfio we configure the vf mac using the kernel nic mac address
			// before we switch to the userspace driver
			if yes, d := hasDriver(addr); yes && !sriovnetworkv1.StringInArray(d, DpdkDrivers) {
				// LinkType is an optional field. Let's fallback to current link type
				// if nothing is specified in the SriovNodePolicy
				linkType := iface.LinkType
				if linkType == "" {
					linkType = ifaceStatus.LinkType
				}
				if strings.EqualFold(linkType, constants.LinkTypeIB) {
					if err = setVfGUID(addr, pfLink); err != nil {
						return err
					}
				} else {
					vfLink, err := vfIsReady(addr)
					if err != nil {
						glog.Errorf("configSriovDevice(): VF link is not ready for device %s %q", addr, err)
						err = RebindVfToDefaultDriver(addr)
						if err != nil {
							glog.Errorf("configSriovDevice(): failed to rebind VF %s %q", addr, err)
							return err
						}

						// Try to check the VF status again
						vfLink, err = vfIsReady(addr)
						if err != nil {
							glog.Errorf("configSriovDevice(): VF link is not ready for device %s %q", addr, err)
							return err
						}
					}
					if err = setVfAdminMac(addr, pfLink, vfLink); err != nil {
						glog.Errorf("configSriovDevice(): fail to configure VF admin mac address for device %s %q", addr, err)
						return err
					}
				}
			}

			if err = unbindDriverIfNeeded(addr, isRdma); err != nil {
				return err
			}

			if dpdkDriver == "" {
				if err := BindDefaultDriver(addr); err != nil {
					glog.Warningf("configSriovDevice(): fail to bind default driver for device %s", addr)
					return err
				}
				// only set MTU for VF with default driver
				if iface.VfGroups[i].Mtu > 0 {
					if err := setNetdevMTU(addr, iface.VfGroups[i].Mtu); err != nil {
						glog.Warningf("configSriovDevice(): fail to set mtu for VF %s: %v", addr, err)
						return err
					}
				}
			} else {
				if err := BindDpdkDriver(addr, dpdkDriver); err != nil {
					glog.Warningf("configSriovDevice(): fail to bind driver %s for device %s", dpdkDriver, addr)
					return err
				}
			}
		}
	}
	// Set PF link up
	pfLink, err := netlink.LinkByName(ifaceStatus.Name)
	if err != nil {
		return err
	}
	if pfLink.Attrs().OperState != netlink.OperUp {
		err = netlink.LinkSetUp(pfLink)
		if err != nil {
			return err
		}
	}
	return nil
}

func setSriovNumVfs(pciAddr string, numVfs int) error {
	glog.V(2).Infof("setSriovNumVfs(): set NumVfs for device %s to %d", pciAddr, numVfs)
	numVfsFilePath := filepath.Join(sysBusPciDevices, pciAddr, numVfsFile)
	bs := []byte(strconv.Itoa(numVfs))
	err := os.WriteFile(numVfsFilePath, []byte("0"), os.ModeAppend)
	if err != nil {
		glog.Warningf("setSriovNumVfs(): fail to reset NumVfs file %s", numVfsFilePath)
		return err
	}
	err = os.WriteFile(numVfsFilePath, bs, os.ModeAppend)
	if err != nil {
		glog.Warningf("setSriovNumVfs(): fail to set NumVfs file %s", numVfsFilePath)
		return err
	}
	return nil
}

func setNetdevMTU(pciAddr string, mtu int) error {
	glog.V(2).Infof("setNetdevMTU(): set MTU for device %s to %d", pciAddr, mtu)
	if mtu <= 0 {
		glog.V(2).Infof("setNetdevMTU(): not set MTU to %d", mtu)
		return nil
	}
	b := backoff.NewConstantBackOff(1 * time.Second)
	err := backoff.Retry(func() error {
		ifaceName, err := dputils.GetNetNames(pciAddr)
		if err != nil {
			glog.Warningf("setNetdevMTU(): fail to get interface name for %s: %s", pciAddr, err)
			return err
		}
		if len(ifaceName) < 1 {
			return fmt.Errorf("setNetdevMTU(): interface name is empty")
		}
		mtuFile := "net/" + ifaceName[0] + "/mtu"
		mtuFilePath := filepath.Join(sysBusPciDevices, pciAddr, mtuFile)
		return os.WriteFile(mtuFilePath, []byte(strconv.Itoa(mtu)), os.ModeAppend)
	}, backoff.WithMaxRetries(b, 10))
	if err != nil {
		glog.Warningf("setNetdevMTU(): fail to write mtu file after retrying: %v", err)
		return err
	}
	return nil
}

func tryGetInterfaceName(pciAddr string) string {
	names, err := dputils.GetNetNames(pciAddr)
	if err != nil || len(names) < 1 {
		return ""
	}
	netDevName := names[0]

	// Switchdev PF and their VFs representors are existing under the same PCI address since kernel 5.8
	// if device is switchdev then return PF name
	for _, name := range names {
		if !isSwitchdev(name) {
			continue
		}
		// Try to get the phys port name, if not exists then fallback to check without it
		// phys_port_name should be in formant p<port-num> e.g p0,p1,p2 ...etc.
		if physPortName, err := GetPhysPortName(name); err == nil {
			if !pfPhysPortNameRe.MatchString(physPortName) {
				continue
			}
		}
		return name
	}

	glog.V(2).Infof("tryGetInterfaceName(): name is %s", netDevName)
	return netDevName
}

func getNetdevMTU(pciAddr string) int {
	glog.V(2).Infof("getNetdevMTU(): get MTU for device %s", pciAddr)
	ifaceName := tryGetInterfaceName(pciAddr)
	if ifaceName == "" {
		return 0
	}
	mtuFile := "net/" + ifaceName + "/mtu"
	mtuFilePath := filepath.Join(sysBusPciDevices, pciAddr, mtuFile)
	data, err := os.ReadFile(mtuFilePath)
	if err != nil {
		glog.Warningf("getNetdevMTU(): fail to read mtu file %s", mtuFilePath)
		return 0
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		glog.Warningf("getNetdevMTU(): fail to convert mtu %s to int", strings.TrimSpace(string(data)))
		return 0
	}
	return mtu
}

func getNetDevMac(ifaceName string) string {
	glog.V(2).Infof("getNetDevMac(): get Mac for device %s", ifaceName)
	macFilePath := filepath.Join(sysClassNet, ifaceName, "address")
	data, err := os.ReadFile(macFilePath)
	if err != nil {
		glog.Warningf("getNetDevMac(): fail to read Mac file %s", macFilePath)
		return ""
	}

	return strings.TrimSpace(string(data))
}

func getNetDevLinkSpeed(ifaceName string) string {
	glog.V(2).Infof("getNetDevLinkSpeed(): get LinkSpeed for device %s", ifaceName)
	speedFilePath := filepath.Join(sysClassNet, ifaceName, "speed")
	data, err := os.ReadFile(speedFilePath)
	if err != nil {
		glog.Warningf("getNetDevLinkSpeed(): fail to read Link Speed file %s", speedFilePath)
		return ""
	}

	return fmt.Sprintf("%s Mb/s", strings.TrimSpace(string(data)))
}

func resetSriovDevice(ifaceStatus sriovnetworkv1.InterfaceExt) error {
	glog.V(2).Infof("resetSriovDevice(): reset SRIOV device %s", ifaceStatus.PciAddress)
	if err := setSriovNumVfs(ifaceStatus.PciAddress, 0); err != nil {
		return err
	}
	if ifaceStatus.LinkType == constants.LinkTypeETH {
		var mtu int
		is := InitialState.GetInterfaceStateByPciAddress(ifaceStatus.PciAddress)
		if is != nil {
			mtu = is.Mtu
		} else {
			mtu = 1500
		}
		glog.V(2).Infof("resetSriovDevice(): reset mtu to %d", mtu)
		if err := setNetdevMTU(ifaceStatus.PciAddress, mtu); err != nil {
			return err
		}
	} else if ifaceStatus.LinkType == constants.LinkTypeIB {
		if err := setNetdevMTU(ifaceStatus.PciAddress, 2048); err != nil {
			return err
		}
	}
	return nil
}

func getVfInfo(pciAddr string, devices []*ghw.PCIDevice) sriovnetworkv1.VirtualFunction {
	driver, err := dputils.GetDriverName(pciAddr)
	if err != nil {
		glog.Warningf("getVfInfo(): unable to parse device driver for device %s %q", pciAddr, err)
	}
	id, err := dputils.GetVFID(pciAddr)
	if err != nil {
		glog.Warningf("getVfInfo(): unable to get VF index for device %s %q", pciAddr, err)
	}
	vf := sriovnetworkv1.VirtualFunction{
		PciAddress: pciAddr,
		Driver:     driver,
		VfID:       id,
	}

	if mtu := getNetdevMTU(pciAddr); mtu > 0 {
		vf.Mtu = mtu
	}
	if name := tryGetInterfaceName(pciAddr); name != "" {
		vf.Name = name
		vf.Mac = getNetDevMac(name)
	}

	for _, device := range devices {
		if pciAddr == device.Address {
			vf.Vendor = device.Vendor.ID
			vf.DeviceID = device.Product.ID
			break
		}
		continue
	}
	return vf
}

func Chroot(path string) (func() error, error) {
	root, err := os.Open("/")
	if err != nil {
		return nil, err
	}

	if err := syscall.Chroot(path); err != nil {
		root.Close()
		return nil, err
	}

	return func() error {
		defer root.Close()
		if err := root.Chdir(); err != nil {
			return err
		}
		return syscall.Chroot(".")
	}, nil
}

func vfIsReady(pciAddr string) (netlink.Link, error) {
	glog.Infof("vfIsReady(): VF device %s", pciAddr)
	var err error
	var vfLink netlink.Link
	err = wait.PollImmediate(time.Second, 10*time.Second, func() (bool, error) {
		vfName := tryGetInterfaceName(pciAddr)
		vfLink, err = netlink.LinkByName(vfName)
		if err != nil {
			glog.Errorf("vfIsReady(): unable to get VF link for device %+v, %q", pciAddr, err)
		}
		return err == nil, nil
	})
	if err != nil {
		return vfLink, err
	}
	return vfLink, nil
}

func setVfAdminMac(vfAddr string, pfLink, vfLink netlink.Link) error {
	glog.Infof("setVfAdminMac(): VF %s", vfAddr)

	vfID, err := dputils.GetVFID(vfAddr)
	if err != nil {
		glog.Errorf("setVfAdminMac(): unable to get VF id %+v %q", vfAddr, err)
		return err
	}

	if err := netlink.LinkSetVfHardwareAddr(pfLink, vfID, vfLink.Attrs().HardwareAddr); err != nil {
		return err
	}

	return nil
}

func unbindDriverIfNeeded(vfAddr string, isRdma bool) error {
	if isRdma {
		glog.Infof("unbindDriverIfNeeded(): unbind driver for %s", vfAddr)
		if err := Unbind(vfAddr); err != nil {
			return err
		}
	}
	return nil
}

func getLinkType(ifaceStatus sriovnetworkv1.InterfaceExt) string {
	glog.V(2).Infof("getLinkType(): Device %s", ifaceStatus.PciAddress)
	if ifaceStatus.Name != "" {
		link, err := netlink.LinkByName(ifaceStatus.Name)
		if err != nil {
			glog.Warningf("getLinkType(): %v", err)
			return ""
		}
		linkType := link.Attrs().EncapType
		if linkType == "ether" {
			return constants.LinkTypeETH
		} else if linkType == "infiniband" {
			return constants.LinkTypeIB
		}
	}

	return ""
}

func setVfGUID(vfAddr string, pfLink netlink.Link) error {
	glog.Infof("setVfGuid(): VF %s", vfAddr)
	vfID, err := dputils.GetVFID(vfAddr)
	if err != nil {
		glog.Errorf("setVfGuid(): unable to get VF id %+v %q", vfAddr, err)
		return err
	}
	guid := generateRandomGUID()
	if err := netlink.LinkSetVfNodeGUID(pfLink, vfID, guid); err != nil {
		return err
	}
	if err := netlink.LinkSetVfPortGUID(pfLink, vfID, guid); err != nil {
		return err
	}
	if err = Unbind(vfAddr); err != nil {
		return err
	}

	return nil
}

func generateRandomGUID() net.HardwareAddr {
	guid := make(net.HardwareAddr, 8)

	// First field is 0x01 - xfe to avoid all zero and all F invalid guids
	guid[0] = byte(1 + rand.Intn(0xfe))

	for i := 1; i < len(guid); i++ {
		guid[i] = byte(rand.Intn(0x100))
	}

	return guid
}

func GetNicSriovMode(pciAddress string) (string, error) {
	glog.V(2).Infof("GetNicSriovMode(): device %s", pciAddress)

	devLink, err := netlink.DevLinkGetDeviceByName("pci", pciAddress)
	if err != nil {
		if errors.Is(err, syscall.ENODEV) {
			// the device doesn't support devlink
			return "", nil
		}
		return "", err
	}

	return devLink.Attrs.Eswitch.Mode, nil
}

func GetPhysSwitchID(name string) (string, error) {
	swIDFile := filepath.Join(sysClassNet, name, "phys_switch_id")
	physSwitchID, err := os.ReadFile(swIDFile)
	if err != nil {
		return "", err
	}
	if physSwitchID != nil {
		return strings.TrimSpace(string(physSwitchID)), nil
	}
	return "", nil
}

func GetPhysPortName(name string) (string, error) {
	devicePortNameFile := filepath.Join(sysClassNet, name, "phys_port_name")
	physPortName, err := os.ReadFile(devicePortNameFile)
	if err != nil {
		return "", err
	}
	if physPortName != nil {
		return strings.TrimSpace(string(physPortName)), nil
	}
	return "", nil
}

func isSwitchdev(name string) bool {
	switchID, err := GetPhysSwitchID(name)
	if err != nil || switchID == "" {
		return false
	}

	return true
}

// IsKernelLockdownMode returns true when kernel lockdown mode is enabled
func IsKernelLockdownMode(chroot bool) bool {
	path := "/sys/kernel/security/lockdown"
	if !chroot {
		path = "/host" + path
	}
	out, err := RunCommand("cat", path)
	glog.V(2).Infof("IsKernelLockdownMode(): %s, %+v", out, err)
	if err != nil {
		return false
	}
	return strings.Contains(out, "[integrity]") || strings.Contains(out, "[confidentiality]")
}

// RunCommand runs a command
func RunCommand(command string, args ...string) (string, error) {
	glog.Infof("RunCommand(): %s %v", command, args)
	var stdout, stderr bytes.Buffer

	cmd := exec.Command(command, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	glog.V(2).Infof("RunCommand(): out:(%s), err:(%v)", stdout.String(), err)
	return stdout.String(), err
}

func HashConfigMap(cm *corev1.ConfigMap) string {
	var keys []string
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	hash := fnv.New128()
	for _, k := range keys {
		hash.Write([]byte(k))
		hash.Write([]byte(cm.Data[k]))
	}
	hashed := hash.Sum(nil)
	return hex.EncodeToString(hashed)
}

func hasMellanoxInterfacesInSpec(ifaceStatuses sriovnetworkv1.InterfaceExts, ifaceSpecs sriovnetworkv1.Interfaces) bool {
	for _, ifaceStatus := range ifaceStatuses {
		if ifaceStatus.Vendor == VendorMellanox {
			for _, iface := range ifaceSpecs {
				if iface.PciAddress == ifaceStatus.PciAddress {
					glog.V(2).Infof("hasMellanoxInterfacesInSpec(): Mellanox device %s (pci: %s) specified in SriovNetworkNodeState spec", ifaceStatus.Name, ifaceStatus.PciAddress)
					return true
				}
			}
		}
	}
	return false
}

// Workaround function to handle a case where the vf default driver is stuck and not able to create the vf kernel interface.
// This function unbind the VF from the default driver and try to bind it again
// bugzilla: https://bugzilla.redhat.com/show_bug.cgi?id=2045087
func RebindVfToDefaultDriver(vfAddr string) error {
	glog.Infof("RebindVfToDefaultDriver(): VF %s", vfAddr)
	if err := Unbind(vfAddr); err != nil {
		return err
	}
	if err := BindDefaultDriver(vfAddr); err != nil {
		glog.Errorf("RebindVfToDefaultDriver(): fail to bind default driver for device %s", vfAddr)
		return err
	}

	glog.Warningf("RebindVfToDefaultDriver(): workaround implemented for VF %s", vfAddr)
	return nil
}

func PrepareNMUdevRule(supportedVfIds []string) error {
	glog.V(2).Infof("PrepareNMUdevRule()")
	dirPath := path.Join(FilesystemRoot, "/host/etc/udev/rules.d")
	filePath := path.Join(dirPath, "10-nm-unmanaged.rules")

	// remove the old unmanaged rules file
	if _, err := os.Stat(filePath); err == nil {
		err = os.Remove(filePath)
		if err != nil {
			glog.Warningf("failed to remove the network manager global unmanaged rule on path %s: %v", filePath, err)
		}
	}

	// create the pf finder script for udev rules
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("/bin/bash", path.Join(FilesystemRoot, udevDisableNM))
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		glog.Errorf("PrepareNMUdevRule(): failed to prepare nmUdevRule, stderr %s: %v", stderr.String(), err)
		return err
	}
	glog.V(2).Infof("PrepareNMUdevRule(): %v", stdout.String())

	//save the device list to use for udev rules
	SupportedVfIds = supportedVfIds
	return nil
}

func AddUdevRule(pfPciAddress string) error {
	glog.V(2).Infof("AddUdevRule(): %s", pfPciAddress)
	pathFile := udevRulesFolder
	udevRuleContent := fmt.Sprintf(nmUdevRule, strings.Join(SupportedVfIds, "|"), pfPciAddress)

	err := os.MkdirAll(pathFile, os.ModePerm)
	if err != nil && !os.IsExist(err) {
		glog.Errorf("AddUdevRule(): failed to create dir %s: %v", pathFile, err)
		return err
	}

	filePath := path.Join(pathFile, fmt.Sprintf("10-nm-disable-%s.rules", pfPciAddress))
	// if the file does not exist or if oldContent != newContent
	// write to file and create it if it doesn't exist
	err = os.WriteFile(filePath, []byte(udevRuleContent), 0666)
	if err != nil {
		glog.Errorf("AddUdevRule(): fail to write file: %v", err)
		return err
	}
	return nil
}

func RemoveUdevRule(pfPciAddress string) error {
	pathFile := udevRulesFolder
	filePath := path.Join(pathFile, fmt.Sprintf("10-nm-disable-%s.rules", pfPciAddress))
	err := os.Remove(filePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
