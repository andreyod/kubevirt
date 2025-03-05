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
 * Copyright 2021 Red Hat, Inc.
 *
 */

package generic

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	v1 "kubevirt.io/api/core/v1"

	cmdv1 "kubevirt.io/kubevirt/pkg/handler-launcher-com/cmd/v1"
	"kubevirt.io/kubevirt/pkg/pointer"
	"kubevirt.io/kubevirt/pkg/util"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/device/hostdevice"
)

const (
	failedCreateGenericHostDevicesFmt = "failed to create generic host-devices: %v"
	AliasPrefix                       = "hostdevice-"
	DefaultDisplayOff                 = false
)

func CreateHostDevices(vmiHostDevices []v1.HostDevice) ([]api.HostDevice, error) {
	return CreateHostDevicesFromPools(vmiHostDevices,
		NewPCIAddressPool(vmiHostDevices), NewMDEVAddressPool(vmiHostDevices), NewUSBAddressPool(vmiHostDevices))
}

func CreateHostDevicesFromPools(vmiHostDevices []v1.HostDevice, pciAddressPool, mdevAddressPool, usbAddressPool hostdevice.AddressPooler) ([]api.HostDevice, error) {
	pciPool := hostdevice.NewBestEffortAddressPool(pciAddressPool)
	mdevPool := hostdevice.NewBestEffortAddressPool(mdevAddressPool)
	usbPool := hostdevice.NewBestEffortAddressPool(usbAddressPool)

	hostDevicesMetaData := createHostDevicesMetadata(vmiHostDevices)
	pciHostDevices, err := hostdevice.CreatePCIHostDevices(hostDevicesMetaData, pciPool)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}
	mdevHostDevices, err := hostdevice.CreateMDEVHostDevices(hostDevicesMetaData, mdevPool, DefaultDisplayOff)
	if err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	hostDevices := append(pciHostDevices, mdevHostDevices...)

	usbHostDevices, err := hostdevice.CreateUSBHostDevices(hostDevicesMetaData, usbPool)
	if err != nil {
		return nil, err
	}

	hostDevices = append(hostDevices, usbHostDevices...)

	if err := validateCreationOfAllDevices(vmiHostDevices, hostDevices); err != nil {
		return nil, fmt.Errorf(failedCreateGenericHostDevicesFmt, err)
	}

	return hostDevices, nil
}

func createHostDevicesMetadata(vmiHostDevices []v1.HostDevice) []hostdevice.HostDeviceMetaData {
	var hostDevicesMetaData []hostdevice.HostDeviceMetaData
	for _, dev := range vmiHostDevices {
		hostDevicesMetaData = append(hostDevicesMetaData, hostdevice.HostDeviceMetaData{
			AliasPrefix:  AliasPrefix,
			Name:         dev.Name,
			ResourceName: dev.DeviceName,
		})
	}
	return hostDevicesMetaData
}

// validateCreationOfAllDevices validates that all specified generic host-devices have a matching host-device.
// On validation failure, an error is returned.
// The validation assumes that the assignment of a device to a specified generic host-device is correct,
// therefore a simple quantity check is sufficient.
func validateCreationOfAllDevices(genericHostDevices []v1.HostDevice, hostDevices []api.HostDevice) error {
	if len(genericHostDevices) != len(hostDevices) {
		return fmt.Errorf(
			"the number of generic host-devices do not match the number of devices:\nGeneric: %v\nDevice: %v",
			genericHostDevices, hostDevices,
		)
	}
	return nil
}

func CreateRootControllers(topology *cmdv1.Topology, hostdevs []v1.HostDevice, gpus []v1.GPU) []api.Controller {

	rootControllers := []api.Controller{}
	// maps the topology NumaCells
	topologyNumaCells := make(map[uint]int) //TODO maybe change to a slice

	numaCells := topology.GetNumaCells()
	if len(numaCells) == 0 {
		return rootControllers
	}
	for _, cell := range numaCells {
		log.Log.Infof("----- got numa cell: %d", cell.Id)
		topologyNumaCells[uint(cell.Id)] = 0
	}

	rootToNuma := listResources(extract(hostdevs, gpus))
	log.Log.Infof("----- rootToNuma mapping: %v", rootToNuma)

	// create PXB controller for each root bus in PCI topology
	indexer := 1
	for _, numaNode := range rootToNuma {
		if _, exists := topologyNumaCells[numaNode]; !exists {
			continue
		}
		newRootController := api.Controller{
			Type:  "pci",
			Index: strconv.Itoa(indexer),
			Model: "pcie-expander-bus", // TODO pci vs pcie
			Target: &api.ControllerTarget{
				NUMANode: pointer.P(numaNode),
			},
		}
		rootControllers = append(rootControllers, newRootController)
		indexer++
	}
	return rootControllers
}

func listResources(resources []string) map[string]uint {
	rootToNuma := make(map[string]uint) // maps the pci root bus to it's numa node
	for _, resource := range resources {
		suffix := "_ROOT_COMPLEX" // TODO make it const
		rootEnvVarName := util.ResourceNameToEnvVar(v1.PCIResourcePrefix, resource) + suffix
		rootComplexString, isSet := os.LookupEnv(rootEnvVarName)
		if !isSet {
			log.Log.Warningf("%s not set for resource %s", rootEnvVarName, resource)
			continue
		}

		rootComplexString = strings.TrimSuffix(rootComplexString, ",")
		log.Log.Infof("----- rootComplexString : %s", rootComplexString)
		rootComplexes := strings.Split(rootComplexString, ",")
		for _, complex := range rootComplexes {
			complexData := strings.Split(complex, "|")
			if len(complexData) != 3 {
				continue
			}
			numaNode, err := strconv.ParseUint(complexData[1], 10, 32)
			if err != nil {
				// invalid numa node
				continue
			}
			rootBus := complexData[0]
			if !strings.HasPrefix(rootBus, "pci") || rootBus == "pci0000:00" {
				continue
			}
			rootToNuma[rootBus] = uint(numaNode)
		}
	}
	return rootToNuma
}

func extract(hostDevices []v1.HostDevice, gpus []v1.GPU) []string {
	var resourceSet = make(map[string]struct{})
	for _, hostDevice := range hostDevices {
		resourceSet[hostDevice.DeviceName] = struct{}{}
	}
	for _, gpu := range gpus {
		resourceSet[gpu.DeviceName] = struct{}{}
	}
	var resources []string
	for resource := range resourceSet {
		resources = append(resources, resource)
	}
	return resources
}
