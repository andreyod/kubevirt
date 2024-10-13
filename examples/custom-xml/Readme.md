# Deploy VM with pre-defined XML

The goal is to allow deployment of a Virtual Machine with custom XML definition.

### Preconditions:

Modify the KubeVirt code with the changes in this [commit](https://github.com/andreyod/kubevirt/commit/929ec11c48e2da432c89f95e3fa8e3ad6cef4163#diff-6edf7af57556f8cb17fd6e5382da7e9858899c4bdf5e3067664b7cb0527cfe35)

Build and deploy the new KubeVirt code.

### Create the pre-defined XML and add it to the configMap:

- Deploy the VM you want to modify and save it's XML definition in a file:
```bash
oc create -f vm.yaml
oc exec -it virt-launcher-vmi-XXXXXX -- bash
virsh list
virsh dumpxml <VM name from the list above>
```
- Modify the XML in the file.

- Copy the new XML into the configMap yaml file under the `xml.properties`. See this [example](https://github.com/andreyod/kubevirt/blob/custom-xml/examples/custom-xml/xmlmap.yaml)
 
- Create the configMap
```bash
oc create -f examples/custom-xml/xmlmap.yaml
```

### Create a VM with pre-defined XML from the configMap:

- Modify the vm.yaml file with the following:
1. Add the `vm.kubevirt.io/xml` label to the VMI labels(Note that the label value is not important)
2. Add the configMap volume to the volumes in the spec.
See this [example](https://github.com/andreyod/kubevirt/blob/custom-xml/examples/custom-xml/vm-custom.yaml)

- Deploy the VM:
```bash
oc create -f vm.yaml
```

Note that all the `domain` values in the vm.yaml will be ignored and the XML pre-defined in the configMap will be deployed.
