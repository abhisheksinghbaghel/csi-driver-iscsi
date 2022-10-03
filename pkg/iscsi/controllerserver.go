/*
Copyright 2019 The Kubernetes Authors.

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

package iscsi

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/elasticsans/armelasticsans"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	klog "k8s.io/klog/v2"
)

const (
	resourceGroupField   = "resourcegroup"
	sanNameField         = "sanname"
	volumeGroupNameField = "volumegroupname"
	volumeNameField      = "volumename"

	pvcNameKey      = "csi.storage.k8s.io/pvc/name"
	pvNameKey       = "csi.storage.k8s.io/pv/name"
	pvcNamespaceKey = "csi.storage.k8s.io/pvc/namespace"
)

type ControllerServer struct {
	Driver       *driver
	VolumeClient *armelasticsans.VolumesClient
}

// CreateVolume provisions an azure file
func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {

	volName := req.GetName()
	if len(volName) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}

	capacityBytes := req.GetCapacityRange().GetRequiredBytes()
	requestGiB := RoundUpGiB(capacityBytes)
	if requestGiB == 0 {
		requestGiB = 4
		klog.Warningf("no quota specified, set as default value(4 GiB)")
	}
	parameters := req.GetParameters()
	if parameters == nil {
		parameters = make(map[string]string)
	}

	var resourceGroup, sanName, volumeGroupName, volumeName string

	// Apply ProvisionerParameters (case-insensitive). We leave validation of
	// the values to the cloud provider.
	for k, v := range parameters {
		switch strings.ToLower(k) {
		case resourceGroupField:
			resourceGroup = v
		case sanNameField:
			sanName = v
		case volumeGroupNameField:
			volumeGroupName = v
		case volumeNameField:
			volumeName = v
		case pvcNameKey:
			// no op
		case pvNameKey:
			// no op
		case pvcNamespaceKey:
			// no op
		default:
			return nil, fmt.Errorf("invalid parameter %q in storage class", k)
		}
	}

	if volumeName == "" {
		volumeName = volName
	}

	// csi:
	// driver: iscsi.csi.k8s.io
	// volumeAttributes:
	//   discoveryCHAPAuth: "false"
	//   iqn: iqn.2022-09.net.windows.core.blob.ElasticSan.es-swq4brjhk3p0:demovolume
	//   iscsiInterface: default
	//   lun: "0"
	//   portals: '[]'
	//   sessionCHAPAuth: "false"
	//   targetPortal: es-swq4brjhk3p0.z35.blob.storage.azure.net:3260
	// volumeHandle: iscsi-data-id

	volumeID := fmt.Sprintf("%s#%s#%s#%s", resourceGroup, sanName, volumeGroupName, volumeName)

	properties := armelasticsans.VolumeProperties{SizeGiB: &requestGiB}
	volParams := armelasticsans.Volume{Properties: &properties}
	pollerResp, err := cs.VolumeClient.BeginCreate(ctx, resourceGroup, sanName, volumeGroupName, volumeName, volParams, nil)
	if err != nil {
		klog.Errorf("CreateVolume(%s) create volume request not accepted: %v", volumeID, err)
		return nil, err
	}

	resp, err := pollerResp.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: time.Second})
	if err != nil {
		klog.Errorf("CreateVolume(%s) failed to create volume: %v", volumeID, err)
		return nil, err
	}

	klog.V(2).Infof("CreateVolume(%s) Succeeded: %v", volumeID, resp)

	// Update Parameters
	parameters["discoveryCHAPAuth"] = "false"
	parameters["iqn"] = *resp.Properties.StorageTarget.TargetIqn
	parameters["iscsiInterface"] = "default"
	parameters["lun"] = "0"
	parameters["portals"] = "[]"
	parameters["sessionCHAPAuth"] = "false"
	parameters["targetPortal"] = *resp.Properties.StorageTarget.TargetPortalHostname + ":" + strconv.FormatUint(uint64(*resp.Properties.StorageTarget.TargetPortalPort), 10)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: capacityBytes,
			VolumeContext: parameters,
		},
	}, nil
}

// get volume info according to volume id
func GetVolumeInfo(id string) (string, string, string, string, error) {
	segments := strings.Split(id, "#")
	if len(segments) < 4 {
		return "", "", "", "", fmt.Errorf("error parsing volume id: %q, should at least contain three #", id)
	}
	return segments[0], segments[1], segments[2], segments[3], nil
}

// DeleteVolume delete an azure file
func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	resourceGroupName, sanName, volumeGroupName, volumeName, err := GetVolumeInfo(volumeID)
	if err != nil {
		// According to CSI Driver Sanity Tester, should succeed when an invalid volume id is used
		klog.Errorf("DeleteVolume(%s) failed to get volume info: %v", volumeID, err)
		return nil, err
	}

	pollerResp, err := cs.VolumeClient.BeginDelete(ctx, resourceGroupName, sanName, volumeGroupName, volumeName, nil)
	if err != nil {
		klog.Errorf("DeleteVolume(%s) delete volume request not accepted: %v", volumeID, err)
		return nil, err
	}

	resp, err := pollerResp.PollUntilDone(ctx, nil)
	if err != nil {
		klog.Errorf("DeleteVolume(%s) failed to delete volume: %v", volumeID, err)
		return nil, err
	}

	klog.V(2).Infof("DeleteVolume(%s) Succeeded: %v", volumeID, resp)

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	nodeID := req.GetNodeId()
	if len(nodeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Node ID not provided")
	}

	klog.V(2).Infof("ControllerPublishVolume: volume(%s) attached to node(%s) successfully", volumeID, nodeID)
	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	nodeID := req.GetNodeId()
	if len(nodeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Node ID not provided")
	}

	klog.V(2).Infof("ControllerUnpublishVolume: volume(%s) detached from node(%s) successfully", volumeID, nodeID)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// ControllerGetCapabilities implements the default GRPC callout.
// Default supports all capabilities.
func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(5).Infof("Using default ControllerGetCapabilities")

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.Driver.cscap,
	}, nil
}

func (cs *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
