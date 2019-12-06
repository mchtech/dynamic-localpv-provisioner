/*
Copyright 2019 The OpenEBS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
This file contains the volume creation and deletion handlers invoked by
the github.com/kubernetes-sigs/sig-storage-lib-external-provisioner/controller.

The handler that are madatory to be implemented:

- Provision - is called by controller to perform custom validation on the PVC
  request and return a valid PV spec. The controller will create the PV object
  using the spec passed to it and bind it to the PVC.

- Delete - is called by controller to perform cleanup tasks on the PV before
  deleting it.

*/

package app

import (
	"fmt"
	"github.com/openebs/maya/pkg/alertlog"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/klog"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
	//pvController "github.com/kubernetes-sigs/sig-storage-lib-external-provisioner/controller"
	mconfig "github.com/openebs/maya/pkg/apis/openebs.io/v1alpha1"
	menv "github.com/openebs/maya/pkg/env/v1alpha1"
	analytics "github.com/openebs/maya/pkg/usage"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	//metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// NewProvisioner will create a new Provisioner object and initialize
//  it with global information used across PV create and delete operations.
func NewProvisioner(stopCh chan struct{}, kubeClient *clientset.Clientset) (*Provisioner, error) {

	namespace := getOpenEBSNamespace() //menv.Get(menv.OpenEBSNamespace)
	if len(strings.TrimSpace(namespace)) == 0 {
		return nil, fmt.Errorf("Cannot start Provisioner: failed to get namespace")
	}

	p := &Provisioner{
		stopCh: stopCh,

		kubeClient:  kubeClient,
		namespace:   namespace,
		helperImage: getDefaultHelperImage(),
		defaultConfig: []mconfig.Config{
			{
				Name:  KeyPVBasePath,
				Value: getDefaultBasePath(),
			},
		},
	}
	p.getVolumeConfig = p.GetVolumeConfig

	return p, nil
}

// SupportsBlock will be used by controller to determine if block mode is
//  supported by the host path provisioner.
func (p *Provisioner) SupportsBlock() bool {
	return true
}

// Provision is invoked by the PVC controller which expect the PV
//  to be provisioned and a valid PV spec returned.
func (p *Provisioner) Provision(opts pvController.VolumeOptions) (*v1.PersistentVolume, error) {
	pvc := opts.PVC
	if pvc.Spec.Selector != nil {
		return nil, fmt.Errorf("claim.Spec.Selector is not supported")
	}
	for _, accessMode := range pvc.Spec.AccessModes {
		if accessMode != v1.ReadWriteOnce {
			return nil, fmt.Errorf("Only support ReadWriteOnce access mode")
		}
	}

	if opts.SelectedNode == nil {
		return nil, fmt.Errorf("configuration error, no node was specified")
	}

	if GetNodeHostname(opts.SelectedNode) == "" {
		return nil, fmt.Errorf("configuration error, node{%v} hostname is empty", opts.SelectedNode.Name)
	}

	name := opts.PVName

	// Create a new Config instance for the PV by merging the
	// default configuration with configuration provided
	// via PVC and the associated StorageClass
	pvCASConfig, err := p.getVolumeConfig(name, pvc)
	if err != nil {
		return nil, err
	}

	//TODO: Determine if hostpath or device based Local PV should be created
	stgType := pvCASConfig.GetStorageType()
	size := resource.Quantity{}
	reqMap := pvc.Spec.Resources.Requests
	if reqMap != nil {
		size = pvc.Spec.Resources.Requests["storage"]
	}
	sendEventOrIgnore(name, size.String(), stgType, analytics.VolumeProvision)
	if stgType == "hostpath" {
		return p.ProvisionHostPath(opts, pvCASConfig)
	}
	if stgType == "device" {
		return p.ProvisionBlockDevice(opts, pvCASConfig)
	}
	if *opts.PVC.Spec.VolumeMode == v1.PersistentVolumeBlock && stgType != "device" {
		return nil, fmt.Errorf("PV with BlockMode is not supported with StorageType %v", stgType)
	}
	alertlog.Logger.Errorw("",
		"eventcode", "cstor.local.pv.provision.failure",
		"msg", "Failed to provision CStor Local PV",
		"rname", opts.PVName,
		"reason", "StorageType not supported",
		"storagetype", stgType,
	)
	return nil, fmt.Errorf("PV with StorageType %v is not supported", stgType)
}

// Delete is invoked by the PVC controller to perform clean-up
//  activities before deleteing the PV object. If reclaim policy is
//  set to not-retain, then this function will create a helper pod
//  to delete the host path from the node.
func (p *Provisioner) Delete(pv *v1.PersistentVolume) (err error) {
	defer func() {
		err = errors.Wrapf(err, "failed to delete volume %v", pv.Name)
	}()
	//Initiate clean up only when reclaim policy is not retain.
	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimRetain {
		//TODO: Determine the type of PV
		pvType := GetLocalPVType(pv)
		size := resource.Quantity{}
		reqMap := pv.Spec.Capacity
		if reqMap != nil {
			size = pv.Spec.Capacity["storage"]
		}

		sendEventOrIgnore(pv.Name, size.String(), pvType, analytics.VolumeDeprovision)
		if pvType == "local-device" {
			err := p.DeleteBlockDevice(pv)
			if err != nil {
				alertlog.Logger.Errorw("",
					"eventcode", "cstor.local.pv.delete.failure",
					"msg", "Failed to delete CStor Local PV",
					"rname", pv.Name,
					"reason", "failed to delete block device",
					"storagetype", pvType,
				)
			}
			return err
		}

		err = p.DeleteHostPath(pv)
		if err != nil {
			alertlog.Logger.Errorw("",
				"eventcode", "cstor.local.pv.delete.failure",
				"msg", "Failed to delete CStor Local PV",
				"rname", pv.Name,
				"reason", "failed to delete host path",
				"storagetype", pvType,
			)
		}
		return err
	}
	klog.Infof("Retained volume %v", pv.Name)
	alertlog.Logger.Infow("",
		"eventcode", "cstor.local.pv.delete.success",
		"msg", "Successfully deleted CStor Local PV",
		"rname", pv.Name,
	)
	return nil
}

// sendEventOrIgnore sends anonymous local-pv provision/delete events
func sendEventOrIgnore(pvName, capacity, stgType, method string) {
	if method == analytics.VolumeProvision {
		stgType = "local-" + stgType
	}
	if menv.Truthy(menv.OpenEBSEnableAnalytics) {
		analytics.New().Build().ApplicationBuilder().
			SetVolumeType(stgType, method).
			SetDocumentTitle(pvName).
			SetLabel(analytics.EventLabelCapacity).
			SetReplicaCount(analytics.LocalPVReplicaCount, method).
			SetCategory(method).
			SetVolumeCapacity(capacity).Send()
	}
}
