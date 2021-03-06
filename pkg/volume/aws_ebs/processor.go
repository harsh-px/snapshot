/*
Copyright 2017 The Kubernetes Authors.

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

package aws_ebs

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/client-go/pkg/api/v1"
	kvol "k8s.io/kubernetes/pkg/volume"

	"github.com/golang/glog"

	tprv1 "github.com/rootfs/snapshot/pkg/apis/tpr/v1"
	"github.com/rootfs/snapshot/pkg/cloudprovider"
	"github.com/rootfs/snapshot/pkg/cloudprovider/providers/aws"
	"github.com/rootfs/snapshot/pkg/volume"
)

type awsEBSPlugin struct {
	cloud *aws.Cloud
}

var _ volume.VolumePlugin = &awsEBSPlugin{}

func RegisterPlugin() volume.VolumePlugin {
	return &awsEBSPlugin{}
}

func GetPluginName() string {
	return "aws_ebs"
}

func (a *awsEBSPlugin) Init(cloud cloudprovider.Interface) {
	a.cloud = cloud.(*aws.Cloud)
}

func (a *awsEBSPlugin) SnapshotCreate(spec *v1.PersistentVolumeSpec) (*tprv1.VolumeSnapshotDataSource, error) {
	if spec == nil || spec.AWSElasticBlockStore == nil {
		return nil, fmt.Errorf("invalid PV spec %v", spec)
	}
	volumeId := spec.AWSElasticBlockStore.VolumeID
	if ind := strings.LastIndex(volumeId, "/"); ind >= 0 {
		volumeId = volumeId[(ind + 1):]
	}
	snapshotOpt := &aws.SnapshotOptions{
		VolumeId: volumeId,
	}
	snapshotId, err := a.cloud.CreateSnapshot(snapshotOpt)
	if err != nil {
		return nil, err
	}
	return &tprv1.VolumeSnapshotDataSource{
		AWSElasticBlockStore: &tprv1.AWSElasticBlockStoreVolumeSnapshotSource{
			SnapshotID: snapshotId,
		},
	}, nil
}

func (a *awsEBSPlugin) SnapshotDelete(src *tprv1.VolumeSnapshotDataSource, _ *v1.PersistentVolume) error {
	if src == nil || src.AWSElasticBlockStore == nil {
		return fmt.Errorf("invalid VolumeSnapshotDataSource: %v", src)
	}
	snapshotId := src.AWSElasticBlockStore.SnapshotID
	_, err := a.cloud.DeleteSnapshot(snapshotId)
	if err != nil {
		return err
	}

	return nil
}

func (a *awsEBSPlugin) SnapshotRestore(snapshotData *tprv1.VolumeSnapshotData, pvc *v1.PersistentVolumeClaim, pvName string, parameters map[string]string) (*v1.PersistentVolumeSource, map[string]string, error) {
	var err error
	var tags = make(map[string]string)
	// retrieve VolumeSnapshotDataSource
	if snapshotData == nil || snapshotData.Spec.AWSElasticBlockStore == nil {
		return nil, nil, fmt.Errorf("failed to retrieve Snapshot spec")
	}
	if pvc == nil {
		return nil, nil, fmt.Errorf("nil pvc")
	}

	snapId := snapshotData.Spec.AWSElasticBlockStore.SnapshotID

	tags["Name"] = kvol.GenerateVolumeName("External Storage", pvName, 255) // AWS tags can have 255 characters

	capacity := pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	requestBytes := capacity.Value()
	// AWS works with gigabytes, convert to GiB with rounding up
	requestGB := int(kvol.RoundUpSize(requestBytes, 1024*1024*1024))
	volumeOptions := &aws.VolumeOptions{
		CapacityGB: requestGB,
		Tags:       tags,
		PVCName:    pvc.Name,
		SnapshotId: snapId,
	}
	// Apply Parameters (case-insensitive). We leave validation of
	// the values to the cloud provider.
	for k, v := range parameters {
		switch strings.ToLower(k) {
		case "type":
			volumeOptions.VolumeType = v
		case "zone":
			volumeOptions.AvailabilityZone = v
		case "iopspergb":
			volumeOptions.IOPSPerGB, err = strconv.Atoi(v)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid iopsPerGB value %q, must be integer between 1 and 30: %v", v, err)
			}
		case "encrypted":
			volumeOptions.Encrypted, err = strconv.ParseBool(v)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid encrypted boolean value %q, must be true or false: %v", v, err)
			}
		case "kmskeyid":
			volumeOptions.KmsKeyId = v
		default:
			return nil, nil, fmt.Errorf("invalid option %q", k)
		}
	}

	// TODO: implement PVC.Selector parsing
	if pvc.Spec.Selector != nil {
		return nil, nil, fmt.Errorf("claim.Spec.Selector is not supported for dynamic provisioning on AWS")
	}

	volumeID, err := a.cloud.CreateDisk(volumeOptions)
	if err != nil {
		glog.V(2).Infof("Error creating EBS Disk volume: %v", err)
		return nil, nil, err
	}
	glog.V(2).Infof("Successfully created EBS Disk volume %s", volumeID)

	labels, err := a.cloud.GetVolumeLabels(volumeID)
	if err != nil {
		// We don't really want to leak the volume here...
		glog.Errorf("error building labels for new EBS volume %q: %v", volumeID, err)
	}

	pv := &v1.PersistentVolumeSource{
		AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
			VolumeID:  string(volumeID),
			FSType:    "ext4",
			Partition: 0,
			ReadOnly:  false,
		},
	}

	return pv, labels, nil

}
