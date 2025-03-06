/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package device_manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util"
	pluginapi "kubevirt.io/kubevirt/pkg/virt-handler/device-manager/deviceplugin/v1beta1"
)

const (
	vfioDevicePath = "/dev/vfio/"
	vfioMount      = "/dev/vfio/vfio"
	pciBasePath    = "/sys/bus/pci/devices"
)

type PCIDevice struct {
	pciID       string
	driver      string
	pciAddress  string
	iommuGroup  string
	numaNode    int
	rootComplex string
}

type PCIDevicePlugin struct {
	*DevicePluginBase
	iommuToPCIMap         map[string]string
	iommuToRootComplexMap map[string]string
}

func (dpi *PCIDevicePlugin) Start(stop <-chan struct{}) (err error) {
	logger := log.DefaultLogger()
	dpi.stop = stop

	err = dpi.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", dpi.socketPath)
	if err != nil {
		return fmt.Errorf("error creating GRPC server socket: %v", err)
	}

	dpi.server = grpc.NewServer([]grpc.ServerOption{}...)
	defer dpi.stopDevicePlugin()

	pluginapi.RegisterDevicePluginServer(dpi.server, dpi)

	errChan := make(chan error, 2)

	go func() {
		errChan <- dpi.server.Serve(sock)
	}()

	err = waitForGRPCServer(dpi.socketPath, connectionTimeout)
	if err != nil {
		return fmt.Errorf("error starting the GRPC server: %v", err)
	}

	err = dpi.register()
	if err != nil {
		return fmt.Errorf("error registering with device plugin manager: %v", err)
	}

	go func() {
		errChan <- dpi.healthCheck()
	}()

	dpi.setInitialized(true)
	logger.Infof("%s device plugin started", dpi.resourceName)
	err = <-errChan

	return err
}

func NewPCIDevicePlugin(pciDevices []*PCIDevice, resourceName string) *PCIDevicePlugin {
	serverSock := SocketPath(strings.Replace(resourceName, "/", "-", -1))
	iommuToPCIMap := make(map[string]string)
	iommuToRootComplexMap := make(map[string]string)

	initHandler()

	devs := constructDPIdevices(pciDevices, iommuToPCIMap, iommuToRootComplexMap)

	dpi := &PCIDevicePlugin{
		DevicePluginBase: &DevicePluginBase{
			devs:         devs,
			initialized:  false,
			lock:         &sync.Mutex{},
			socketPath:   serverSock,
			devicePath:   vfioDevicePath,
			resourceName: resourceName,
			deviceRoot:   util.HostRootMount,
			health:       make(chan deviceHealth),
			done:         make(chan struct{}),
			deregistered: make(chan struct{}),
		},
		iommuToPCIMap:         iommuToPCIMap,
		iommuToRootComplexMap: iommuToRootComplexMap,
	}
	return dpi
}

func constructDPIdevices(pciDevices []*PCIDevice, iommuToPCIMap map[string]string, iommuToRootComplexMap map[string]string) (devs []*pluginapi.Device) {
	for _, pciDevice := range pciDevices {
		iommuToPCIMap[pciDevice.iommuGroup] = pciDevice.pciAddress
		iommuToRootComplexMap[pciDevice.iommuGroup] = pciDevice.rootComplex + "|" + fmt.Sprintf("%d", pciDevice.numaNode)
		dpiDev := &pluginapi.Device{
			ID:     pciDevice.iommuGroup,
			Health: pluginapi.Healthy,
		}
		if pciDevice.numaNode >= 0 {
			numaInfo := &pluginapi.NUMANode{
				ID: int64(pciDevice.numaNode),
			}
			dpiDev.Topology = &pluginapi.TopologyInfo{
				Nodes: []*pluginapi.NUMANode{numaInfo},
			}
		}
		devs = append(devs, dpiDev)
	}
	return
}

func (dpi *PCIDevicePlugin) Allocate(_ context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resourceNameEnvVar := util.ResourceNameToEnvVar(v1.PCIResourcePrefix, dpi.resourceName)
	allocatedDevices := []string{}
	allocatedDevicesAffinity := []string{}
	resp := new(pluginapi.AllocateResponse)
	containerResponse := new(pluginapi.ContainerAllocateResponse)

	for _, request := range r.ContainerRequests {
		deviceSpecs := make([]*pluginapi.DeviceSpec, 0)
		for _, devID := range request.DevicesIDs {
			// translate device's iommu group to its pci address
			devPCIAddress, exist := dpi.iommuToPCIMap[devID]
			if !exist {
				continue
			}
			allocatedDevices = append(allocatedDevices, devPCIAddress)
			deviceSpecs = append(deviceSpecs, formatVFIODeviceSpecs(devID)...)
			devRootComplex, exist := dpi.iommuToRootComplexMap[devID]
			if !exist {
				continue
			}
			allocatedDevicesAffinity = append(allocatedDevicesAffinity, devRootComplex+"|"+devPCIAddress)

		}
		containerResponse.Devices = deviceSpecs
		envVar := make(map[string]string)
		envVar[resourceNameEnvVar] = strings.Join(allocatedDevices, ",")
		// TODO util const or func for "_ROOT_COMPLEX"
		resourceAffinityEnvVar := resourceNameEnvVar + "_ROOT_COMPLEX"
		envVar[resourceAffinityEnvVar] = strings.Join(allocatedDevicesAffinity, ",")

		//TEMP testing
		// envVar[resourceAffinityEnvVar] = "pci0000:00|-1,pci0000:00|0,pci0000:01|-1,pci0000:02|0,pci0000:02|0,pci0000:03|1,pci0000:04|0,"
		// envVar[resourceAffinityEnvVar] = "pci0000:02|0|0000:00:04.0"

		containerResponse.Envs = envVar
		resp.ContainerResponses = append(resp.ContainerResponses, containerResponse)
	}
	return resp, nil
}

func (dpi *PCIDevicePlugin) healthCheck() error {
	logger := log.DefaultLogger()
	monitoredDevices := make(map[string]string)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to creating a fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	// This way we don't have to mount /dev from the node
	devicePath := filepath.Join(dpi.deviceRoot, dpi.devicePath)

	// Start watching the files before we check for their existence to avoid races
	dirName := filepath.Dir(devicePath)
	err = watcher.Add(dirName)
	if err != nil {
		return fmt.Errorf("failed to add the device root path to the watcher: %v", err)
	}

	_, err = os.Stat(devicePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not stat the device: %v", err)
		}
	}

	// probe all devices
	for _, dev := range dpi.devs {
		vfioDevice := filepath.Join(devicePath, dev.ID)
		err = watcher.Add(vfioDevice)
		if err != nil {
			return fmt.Errorf("failed to add the device %s to the watcher: %v", vfioDevice, err)
		}
		monitoredDevices[vfioDevice] = dev.ID
	}

	dirName = filepath.Dir(dpi.socketPath)
	err = watcher.Add(dirName)

	if err != nil {
		return fmt.Errorf("failed to add the device-plugin kubelet path to the watcher: %v", err)
	}
	_, err = os.Stat(dpi.socketPath)
	if err != nil {
		return fmt.Errorf("failed to stat the device-plugin socket: %v", err)
	}

	for {
		select {
		case <-dpi.stop:
			return nil
		case err := <-watcher.Errors:
			logger.Reason(err).Errorf("error watching devices and device plugin directory")
		case event := <-watcher.Events:
			logger.V(4).Infof("health Event: %v", event)
			if monDevId, exist := monitoredDevices[event.Name]; exist {
				// Health in this case is if the device path actually exists
				if event.Op == fsnotify.Create {
					logger.Infof("monitored device %s appeared", dpi.resourceName)
					dpi.health <- deviceHealth{
						DevId:  monDevId,
						Health: pluginapi.Healthy,
					}
				} else if (event.Op == fsnotify.Remove) || (event.Op == fsnotify.Rename) {
					logger.Infof("monitored device %s disappeared", dpi.resourceName)
					dpi.health <- deviceHealth{
						DevId:  monDevId,
						Health: pluginapi.Unhealthy,
					}
				}
			} else if event.Name == dpi.socketPath && event.Op == fsnotify.Remove {
				logger.Infof("device socket file for device %s was removed, kubelet probably restarted.", dpi.resourceName)
				return nil
			}
		}
	}
}

func discoverPermittedHostPCIDevices(supportedPCIDeviceMap map[string]string) map[string][]*PCIDevice {
	initHandler()

	pciDevicesMap := make(map[string][]*PCIDevice)
	err := filepath.Walk(pciBasePath, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		pciID, err := Handler.GetDevicePCIID(pciBasePath, info.Name())
		if err != nil {
			log.DefaultLogger().Reason(err).Errorf("failed get vendor:device ID for device: %s", info.Name())
			return nil
		}
		if resourceName, supported := supportedPCIDeviceMap[pciID]; supported {
			// check device driver
			driver, err := Handler.GetDeviceDriver(pciBasePath, info.Name())
			if err != nil || driver != "vfio-pci" {
				return nil
			}

			pcidev := &PCIDevice{
				pciID:      pciID,
				pciAddress: info.Name(),
			}
			iommuGroup, err := Handler.GetDeviceIOMMUGroup(pciBasePath, info.Name())
			if err != nil {
				return nil
			}
			pcidev.iommuGroup = iommuGroup
			pcidev.driver = driver
			pcidev.numaNode = Handler.GetDeviceNumaNode(pciBasePath, info.Name())
			pcidev.rootComplex = Handler.GetDeviceRootComplex(pciBasePath, info.Name())
			pciDevicesMap[resourceName] = append(pciDevicesMap[resourceName], pcidev)
		}
		return nil
	})
	if err != nil {
		log.DefaultLogger().Reason(err).Errorf("failed to discover host devices")
	}
	return pciDevicesMap
}
