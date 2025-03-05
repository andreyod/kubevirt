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
 * Copyright 2023 Red Hat, Inc.
 *
 */

package virtwrap

import (
	"encoding/xml"
	"fmt"
	"os"
	"strconv"
	"strings"

	"kubevirt.io/kubevirt/pkg/network/namescheme"

	"libvirt.org/go/libvirt"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util"

	virtnetlink "kubevirt.io/kubevirt/pkg/network/link"
	netvmispec "kubevirt.io/kubevirt/pkg/network/vmispec"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/cli"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/converter"
	wraputil "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/util"
)

type vmConfigurator interface {
	SetupPodNetworkPhase2(domain *api.Domain, networksToPlug []v1.Network) error
}

type virtIOInterfaceManager struct {
	dom          cli.VirDomain
	configurator vmConfigurator
}

const (
	// ReservedInterfaces represents the number of interfaces the domain
	// should reserve for future hotplug additions.
	ReservedInterfaces = 4

	libvirtInterfaceLinkStateDown = "down"
)

func newVirtIOInterfaceManager(
	libvirtClient cli.VirDomain,
	configurator vmConfigurator,
) *virtIOInterfaceManager {
	return &virtIOInterfaceManager{
		dom:          libvirtClient,
		configurator: configurator,
	}
}

func (vim *virtIOInterfaceManager) hotplugVirtioInterface(vmi *v1.VirtualMachineInstance, currentDomain *api.Domain, updatedDomain *api.Domain) error {
	for _, network := range networksToHotplugWhoseInterfacesAreNotInTheDomain(vmi, indexedDomainInterfaces(currentDomain)) {
		log.Log.Infof("will hot plug %s", network.Name)

		if err := vim.configurator.SetupPodNetworkPhase2(updatedDomain, []v1.Network{network}); err != nil {
			return err
		}

		relevantIface := lookupDomainInterfaceByName(updatedDomain.Spec.Devices.Interfaces, network.Name)
		if relevantIface == nil {
			return fmt.Errorf("could not retrieve the api.Interface object from the dummy domain")
		}

		ifaceMAC := ""
		if relevantIface.MAC != nil {
			ifaceMAC = relevantIface.MAC.MAC
		}
		log.Log.Infof("will hot plug %q with MAC %q", network.Name, ifaceMAC)
		ifaceXML, err := xml.Marshal(relevantIface)
		if err != nil {
			return err
		}

		if err := vim.dom.AttachDeviceFlags(strings.ToLower(string(ifaceXML)), affectDeviceLiveAndConfigLibvirtFlags); err != nil {
			log.Log.Reason(err).Errorf("libvirt failed to attach interface %s: %v", network.Name, err)
			return err
		}
	}
	return nil
}

func (vim *virtIOInterfaceManager) updateDomainLinkState(currentDomain, desiredDomain *api.Domain) error {

	currentDomainIfacesByAlias := indexedDomainInterfaces(currentDomain)
	for _, desiredIface := range desiredDomain.Spec.Devices.Interfaces {
		curIface, ok := currentDomainIfacesByAlias[desiredIface.Alias.GetName()]
		if !ok {
			continue
		}

		if !isLinkStateEqual(curIface, desiredIface) {
			curIface.LinkState = desiredIface.LinkState
			if err := vim.updateIfaceInDomain(&curIface); err != nil {
				return err
			}
		}

	}
	return nil
}

func (vim *virtIOInterfaceManager) updateIfaceInDomain(domIfaceToUpdate *api.Interface) error {
	log.Log.Infof("preparing to update link state to interface %q", domIfaceToUpdate.Alias.GetName())
	ifaceXML, err := xml.Marshal(domIfaceToUpdate)
	if err != nil {
		return err
	}

	if err = vim.dom.UpdateDeviceFlags(strings.ToLower(string(ifaceXML)), affectDeviceLiveAndConfigLibvirtFlags); err != nil {
		log.Log.Reason(err).Errorf("libvirt failed to set link state to interface %s , %v", domIfaceToUpdate.Alias.GetName(), err)
		return err
	}
	return nil
}

func (vim *virtIOInterfaceManager) hotUnplugVirtioInterface(vmi *v1.VirtualMachineInstance, currentDomain *api.Domain) error {
	for _, domainIface := range interfacesToHotUnplug(vmi.Spec.Domain.Devices.Interfaces, vmi.Spec.Networks, currentDomain.Spec.Devices.Interfaces) {
		log.Log.Infof("preparing to hot-unplug %s", domainIface.Alias.GetName())

		ifaceXML, err := xml.Marshal(domainIface)
		if err != nil {
			return err
		}

		if derr := vim.dom.DetachDeviceFlags(strings.ToLower(string(ifaceXML)), affectDeviceLiveAndConfigLibvirtFlags); derr != nil {
			log.Log.Reason(derr).Errorf("libvirt failed to detach interface %s: %v", domainIface.Alias.GetName(), derr)
			return derr
		}
	}
	return nil
}

func interfacesToHotUnplug(vmiSpecInterfaces []v1.Interface, vmiSpecNets []v1.Network, domainSpecInterfaces []api.Interface) []api.Interface {
	ifaces2remove := netvmispec.FilterInterfacesSpec(vmiSpecInterfaces, func(iface v1.Interface) bool {
		return iface.State == v1.InterfaceStateAbsent
	})

	networksByName := netvmispec.IndexNetworkSpecByName(vmiSpecNets)
	var domainIfacesToRemove []api.Interface
	for _, vmiIface := range ifaces2remove {
		if domainIface := lookupDomainInterfaceByName(domainSpecInterfaces, vmiIface.Name); domainIface != nil {
			if hasDeviceWithHashedTapName(domainIface.Target, vmiIface, networksByName[vmiIface.Name]) {
				domainIfacesToRemove = append(domainIfacesToRemove, *domainIface)
			}
		}
	}
	return domainIfacesToRemove
}

func hasDeviceWithHashedTapName(target *api.InterfaceTarget, vmiIface v1.Interface, vmiNet v1.Network) bool {
	return target != nil &&
		target.Device == virtnetlink.GenerateTapDeviceName(namescheme.GenerateHashedInterfaceName(vmiIface.Name), vmiNet)
}

func lookupDomainInterfaceByName(domainIfaces []api.Interface, networkName string) *api.Interface {
	for _, iface := range domainIfaces {
		if iface.Alias.GetName() == networkName {
			return &iface
		}
	}
	return nil
}

func networksToHotplugWhoseInterfacesAreNotInTheDomain(vmi *v1.VirtualMachineInstance, indexedDomainIfaces map[string]api.Interface) []v1.Network {
	var networksToHotplug []v1.Network
	interfacesToHoplug := netvmispec.IndexInterfaceStatusByName(
		vmi.Status.Interfaces,
		func(ifaceStatus v1.VirtualMachineInstanceNetworkInterface) bool {
			_, exists := indexedDomainIfaces[ifaceStatus.Name]
			vmiSpecIface := netvmispec.LookupInterfaceByName(vmi.Spec.Domain.Devices.Interfaces, ifaceStatus.Name)

			return netvmispec.ContainsInfoSource(
				ifaceStatus.InfoSource, netvmispec.InfoSourceMultusStatus,
			) && !exists && vmiSpecIface.State != v1.InterfaceStateAbsent && vmiSpecIface.SRIOV == nil
		},
	)

	for netName, network := range netvmispec.IndexNetworkSpecByName(vmi.Spec.Networks) {
		if _, isAttachmentToBeHotplugged := interfacesToHoplug[netName]; isAttachmentToBeHotplugged {
			networksToHotplug = append(networksToHotplug, network)
		}
	}

	return networksToHotplug
}

func indexedDomainInterfaces(domain *api.Domain) map[string]api.Interface {
	domainInterfaces := map[string]api.Interface{}
	for _, iface := range domain.Spec.Devices.Interfaces {
		domainInterfaces[iface.Alias.GetName()] = iface
	}
	return domainInterfaces
}

// withNetworkIfacesResources adds network interfaces as placeholders to the domain spec
// to trigger the addition of the dependent resources/devices (e.g. PCI controllers).
// As its last step, it reads the generated configuration and removes the network interfaces
// so none will be created with the domain creation.
// The dependent devices are left in the configuration, to allow future hotplug.
func withNetworkIfacesResources(vmi *v1.VirtualMachineInstance, domainSpec *api.DomainSpec, f func(v *v1.VirtualMachineInstance, s *api.DomainSpec) (cli.VirDomain, error)) (cli.VirDomain, error) {
	domainSpecWithIfacesResource := appendPlaceholderInterfacesToTheDomain(vmi, domainSpec)
	dom, err := f(vmi, domainSpecWithIfacesResource)
	if err != nil {
		return nil, err
	}

	// TODO not sure what to do with the condition below

	if len(domainSpec.Devices.Interfaces) == len(domainSpecWithIfacesResource.Devices.Interfaces) {
		return dom, nil
	}

	domainSpecWithoutIfacePlaceholders, err := wraputil.GetDomainSpecWithFlags(dom, libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return nil, err
	}

	// wire up PCI topology root buses
	domainSpecWithoutIfacePlaceholders.Devices.Controllers = connectHostDevicesToRootComplex(vmi, domainSpecWithoutIfacePlaceholders.Devices)

	domainSpecWithoutIfacePlaceholders.Devices.Interfaces = domainSpec.Devices.Interfaces
	// Only the devices are taken into account because some parameters are not assured to be returned when
	// getting the domain spec (e.g. the `qemu:commandline` section).
	domainSpecWithoutIfacePlaceholders.Devices.DeepCopyInto(&domainSpec.Devices)

	return f(vmi, domainSpec)
}

func connectHostDevicesToRootComplex(vmi *v1.VirtualMachineInstance, devices api.Devices) []api.Controller {
	controllers := devices.Controllers
	log.Log.Infof("------- hostDevs %v", devices.HostDevices)

	// map PXBs. if nothing - return
	numaToRootComplexMap := make(map[uint][]string) // key - numaNode, value - array of PXB indexes
	for _, controller := range controllers {
		if controller.Model == "pcie-expander-bus" {
			if controller.Target == nil || controller.Target.NUMANode == nil {
				continue
			}
			numaNode := *controller.Target.NUMANode
			numaToRootComplexMap[numaNode] = append(numaToRootComplexMap[numaNode], controller.Index)
		}
	}
	if len(numaToRootComplexMap) == 0 { // No root complexes to connect
		log.Log.Info("------ No root complexes to connect. Done")
		return controllers
	}

	// get env var. map
	numaToDevices := listResources(extract(vmi.Spec.Domain.Devices.HostDevices, vmi.Spec.Domain.Devices.GPUs))
	log.Log.Infof("----- numaToDevices : %v", numaToDevices)

	// Start from the HostDev, go up to the `pcie-root-port` then change the port address to PXB index
	// map Hostdevs (type='pci' <driver name='vfio'). if nothing - return
	PCItoRootIndex := make(map[string]string)
	for node, devGroups := range numaToDevices {
		// value is an arrays of devices to connect
		rootsPerNuma := numaToRootComplexMap[node]
		if len(devGroups) != len(rootsPerNuma) {
			// not enought PXBs was created
			// TODO log
			continue
		}
		for i, group := range devGroups {
			PCIinGroup := strings.Split(group, ",")
			for _, address := range PCIinGroup {
				PCItoRootIndex[address] = rootsPerNuma[i]
			}
		}
	}

	//return controllers
	return attachDevicesToRoot(devices.HostDevices, controllers, PCItoRootIndex)
}

func attachDevicesToRoot(devices []api.HostDevice, controllers []api.Controller, PCItoRootIndex map[string]string) []api.Controller {

	// map controller to it's index
	controllerToIndex := make(map[string]api.Controller)
	for _, controller := range controllers {
		controllerToIndex[controller.Index] = controller
	}
	// get the port controller from the map and change it's address to pxb index
	for _, device := range devices {
		sourceAddress := device.Source.Address
		sourceAddrStr := fmt.Sprintf("%s:%s:%s.%s", sourceAddress.Domain[2:], sourceAddress.Bus[2:], sourceAddress.Slot[2:], sourceAddress.Function[2:])
		if rootIndex, exists := PCItoRootIndex[sourceAddrStr]; exists {
			// good case. we need to connect this hostdev
			log.Log.Infof("--------- going to attach device %s to root index: %s", sourceAddrStr, rootIndex)
			bus := device.Address.Bus[2:]
			attachPortToRootComplex(rootIndex, bus, controllerToIndex)
		}
	}

	return controllers
}

func attachPortToRootComplex(rootIndex string, bus string, controllerToIndex map[string]api.Controller) {
	parentIndex := getParentIndex(bus)
	if parentIndex == "" {
		return
	}
	log.Log.Infof("--------- attachPortToRoorComplex parentIndex: %s; controllerToIndex: %v", parentIndex, controllerToIndex)
	devContr, exists := controllerToIndex[parentIndex]
	if !exists {
		log.Log.Infof("--------- controller for the bus is not found")
		return
	}
	log.Log.Infof("--------- controller for the bus is: (%v)", devContr)
	model := devContr.Model
	if model != "pcie-root-port" {
		log.Log.Infof("--------- controller for the bus is not pcie-root-port but: %s", model)
		if model == "pcie-root" {
			log.Log.Infof("--------- controller is pcie-root. Done.")
			return
		}
		//parentBus := devContr.Address.Bus[2:] // TODO: need to convert to decimal
		parentIndex = getParentIndex(devContr.Address.Bus[2:])
		if parentIndex == "" {
			return
		}
		attachPortToRootComplex(rootIndex, parentIndex, controllerToIndex)
	} else { // change the port bus to attach to PXB
		// convert rootIndex to Hexadecimal
		output, err := strconv.ParseInt(rootIndex, 10, 64)
		if err != nil {
			//TODO
			log.Log.Infof("--------- failed to parse the rootIndex: %v", err)
			return
		}
		log.Log.Infof("--------- rootIndex in Hex: %s", strconv.FormatInt(output, 16))

		devContr.Address.Bus = strings.Replace(devContr.Address.Bus, devContr.Address.Bus[2:], strconv.FormatInt(output, 16), 1)
		log.Log.Infof("--------- attached controller: %s to new address bus: %s", devContr.Index, devContr.Address.Bus)
	}

}

func getParentIndex(bus string) string {
	log.Log.Infof("--------- checking bus: %s", bus)
	output, err := strconv.ParseUint(bus, 16, 64)
	if err != nil {
		//TODO
		log.Log.Infof("--------- failed to ParseUint. output: (%v)", output)
		return ""
	}
	log.Log.Infof("--------- output: (%v)", output)
	return strconv.FormatUint(uint64(output), 10)
}

func listResources(resources []string) map[uint][]string {

	rootToNuma := make(map[string]uint)
	rootToDevices := make(map[string][]string)

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
			rootToDevices[rootBus] = append(rootToDevices[rootBus], complexData[2])
		}
	}

	log.Log.Infof("----- rootToNuma : %v", rootToNuma)
	log.Log.Infof("----- rootToDevices : %v", rootToDevices)

	numaToDevices := make(map[uint][]string) // the key is numaNode, the value is groups of PCI addreses for single root complex
	for rootBus, numaNode := range rootToNuma {
		numaToDevices[numaNode] = append(numaToDevices[numaNode], strings.Join(rootToDevices[rootBus], ","))
	}

	return numaToDevices
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

func appendPlaceholderInterfacesToTheDomain(vmi *v1.VirtualMachineInstance, domainSpec *api.DomainSpec) *api.DomainSpec {
	if len(vmi.Spec.Domain.Devices.Interfaces) == 0 {
		return domainSpec
	}
	if val := vmi.Annotations[v1.PlacePCIDevicesOnRootComplex]; val == "true" {
		return domainSpec
	}
	domainSpecWithIfacesResource := domainSpec.DeepCopy()
	interfacePlaceholderCount := ReservedInterfaces - len(vmi.Spec.Domain.Devices.Interfaces)
	for i := 0; i < interfacePlaceholderCount; i++ {
		domainSpecWithIfacesResource.Devices.Interfaces = append(
			domainSpecWithIfacesResource.Devices.Interfaces,
			newInterfacePlaceholder(i, converter.InterpretTransitionalModelType(vmi.Spec.Domain.Devices.UseVirtioTransitional, vmi.Spec.Architecture)),
		)
	}
	return domainSpecWithIfacesResource
}

func newInterfacePlaceholder(index int, modelType string) api.Interface {
	return api.Interface{
		Type:  "ethernet",
		Model: &api.Model{Type: modelType},
		Target: &api.InterfaceTarget{
			Device:  fmt.Sprintf("placeholder-%d", index),
			Managed: "no",
		},
	}
}

func isLinkStateEqual(iface1, iface2 api.Interface) bool {
	if iface1.LinkState == nil && iface2.LinkState == nil {
		return true
	}

	if iface1.LinkState == nil || iface2.LinkState == nil {
		return false
	}

	return iface1.LinkState.State == iface2.LinkState.State
}
