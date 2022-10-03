/*
Copyright 2021 The Kubernetes Authors.

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
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/elasticsans/armelasticsans"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	klog "k8s.io/klog/v2"

	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

const (
	GiB = 1024 * 1024 * 1024
)

func NewDefaultIdentityServer(d *driver) *IdentityServer {
	return &IdentityServer{
		Driver: d,
	}
}

// GetCloudProviderFromClient get Azure Cloud Provider
func GetAuthConfig() (*azure.Config, error) {
	credFile := "/etc/kubernetes/azure.json"
	credFileConfig, err := os.Open(credFile)
	if err != nil {
		err = fmt.Errorf("failed to load cloud config from file %q: %v", credFile, err)
		klog.Errorf(err.Error())
		return nil, err
	}
	defer credFileConfig.Close()

	config, err := azure.ParseConfig(credFileConfig)
	if err != nil {
		err = fmt.Errorf("failed to parse cloud config file %q: %v", credFile, err)
		klog.Errorf(err.Error())
		return nil, err
	}

	return config, nil
}

func NewControllerServer(d *driver) *ControllerServer {

	config, err := GetAuthConfig()
	if err != nil {
		klog.Errorf("Failed to read auth config %v", err.Error())
	}

	klog.V(2).Infof("Using client ID: %s", config.UserAssignedIdentityID)
	clientID := azidentity.ClientID(config.UserAssignedIdentityID)
	miCred :=  &azidentity.ManagedIdentityCredentialOptions{ID: clientID}

	cred, err := azidentity.NewManagedIdentityCredential(miCred)
	if err != nil {
		klog.Errorf("Failed to read default creds %v", err.Error())
	}

	client, err := armelasticsans.NewVolumesClient(config.SubscriptionID, cred, nil)
	if err != nil {
		klog.Errorf("Failed to create volume client %v", err.Error())
	}

	return &ControllerServer{
		Driver:       d,
		VolumeClient: client,
	}
}

func NewControllerServiceCapability(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

func ParseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("Invalid endpoint: %v", ep)
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(3).Infof("GRPC call: %s", info.FullMethod)
	klog.V(5).Infof("GRPC request: %s", protosanitizer.StripSecrets(req))
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("GRPC error: %v", err)
	} else {
		klog.V(5).Infof("GRPC response: %s", protosanitizer.StripSecrets(resp))
	}
	return resp, err
}

// RoundUpBytes rounds up the volume size in bytes upto multiplications of GiB
// in the unit of Bytes
func RoundUpBytes(volumeSizeBytes int64) int64 {
	return roundUpSize(volumeSizeBytes, GiB) * GiB
}

// RoundUpGiB rounds up the volume size in bytes upto multiplications of GiB
// in the unit of GiB
func RoundUpGiB(volumeSizeBytes int64) int64 {
	return roundUpSize(volumeSizeBytes, GiB)
}

// BytesToGiB conversts Bytes to GiB
func BytesToGiB(volumeSizeBytes int64) int64 {
	return volumeSizeBytes / GiB
}

// GiBToBytes converts GiB to Bytes
func GiBToBytes(volumeSizeGiB int64) int64 {
	return volumeSizeGiB * GiB
}

// roundUpSize calculates how many allocation units are needed to accommodate
// a volume of given size. E.g. when user wants 1500MiB volume, while Azure File
// allocates volumes in gibibyte-sized chunks,
// RoundUpSize(1500 * 1024*1024, 1024*1024*1024) returns '2'
// (2 GiB is the smallest allocatable volume that can hold 1500MiB)
func roundUpSize(volumeSizeBytes int64, allocationUnitBytes int64) int64 {
	roundedUp := volumeSizeBytes / allocationUnitBytes
	if volumeSizeBytes%allocationUnitBytes > 0 {
		roundedUp++
	}
	return roundedUp
}
