/*
Copyright 2018 The Kubernetes Authors.

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

package driver

import (
	"context"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog"
	k8sexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"

	"github.com/juicedata/juicefs-csi-driver/pkg/juicefs"
	"github.com/juicedata/juicefs-csi-driver/pkg/k8sclient"
	"github.com/juicedata/juicefs-csi-driver/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	nodeCaps = []csi.NodeServiceCapability_RPC_Type{csi.NodeServiceCapability_RPC_GET_VOLUME_STATS}
)

const defaultCheckTimeout = 2 * time.Second

type nodeService struct {
	mount.SafeFormatAndMount
	juicefs   juicefs.Interface
	nodeID    string
	k8sClient *k8sclient.K8sClient
	metrics   *nodeMetrics
}

type nodeMetrics struct {
	volumeErrors    prometheus.Counter
	volumeDelErrors prometheus.Counter
}

func newNodeMetrics(reg prometheus.Registerer) *nodeMetrics {
	metrics := &nodeMetrics{}
	metrics.volumeErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "volume_errors",
		Help: "number of volume errors",
	})
	reg.MustRegister(metrics.volumeErrors)
	metrics.volumeDelErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "volume_del_errors",
		Help: "number of volume delete errors",
	})
	reg.MustRegister(metrics.volumeDelErrors)
	return metrics
}

func newNodeService(nodeID string, k8sClient *k8sclient.K8sClient, reg prometheus.Registerer) (*nodeService, error) {
	mounter := &mount.SafeFormatAndMount{
		Interface: mount.New(""),
		Exec:      k8sexec.New(),
	}
	metrics := newNodeMetrics(reg)
	jfsProvider := juicefs.NewJfsProvider(mounter, k8sClient)
	return &nodeService{
		SafeFormatAndMount: *mounter,
		juicefs:            jfsProvider,
		nodeID:             nodeID,
		k8sClient:          k8sClient,
		metrics:            metrics,
	}, nil
}

// NodeStageVolume is called by the CO prior to the volume being consumed by any workloads on the node by `NodePublishVolume`
func (d *nodeService) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeUnstageVolume is a reverse operation of `NodeStageVolume`
func (d *nodeService) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodePublishVolume is called by the CO when a workload that wants to use the specified volume is placed (scheduled) on a node
func (d *nodeService) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	// WARNING: debug only, secrets included
	klog.V(6).Infof("NodePublishVolume: called with args %+v", req)

	volumeID := req.GetVolumeId()
	klog.V(5).Infof("NodePublishVolume: volume_id is %s", volumeID)

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}
	klog.V(5).Infof("NodePublishVolume: volume_capability is %s", volCap)

	if !isValidVolumeCapabilities([]*csi.VolumeCapability{volCap}) {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not supported")
	}

	var pv *corev1.PersistentVolume
	var err error
	if d.k8sClient != nil {
		pv, err = d.k8sClient.GetPersistentVolume(ctx, volumeID)
		if k8serrors.IsNotFound(err) {
			klog.Warningf("volumeHandle %s is not the same as pv name, mount pod might not be created", volumeID)
		}
	}

	klog.V(5).Infof("NodePublishVolume: creating dir %s", target)
	if err := d.juicefs.CreateTarget(ctx, target); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not create dir %q: %v", target, err)
	}

	options := []string{}
	if req.GetReadonly() || req.VolumeCapability.AccessMode.GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		options = append(options, "ro")
	}
	if m := volCap.GetMount(); m != nil {
		// get mountOptions from PV.spec.mountOptions or StorageClass.mountOptions
		options = append(options, m.MountFlags...)
	}

	volCtx := req.GetVolumeContext()
	klog.V(5).Infof("NodePublishVolume: volume context: %v", volCtx)

	secrets := req.Secrets
	mountOptions := []string{}
	// get mountOptions from PV.volumeAttributes or StorageClass.parameters
	if opts, ok := volCtx["mountOptions"]; ok {
		mountOptions = strings.Split(opts, ",")
	}
	mountOptions = append(mountOptions, options...)

	klog.V(5).Infof("NodePublishVolume: mounting juicefs with secret %+v, options %v", reflect.ValueOf(secrets).MapKeys(), mountOptions)
	jfs, err := d.juicefs.JfsMount(ctx, volumeID, target, secrets, volCtx, mountOptions)
	if err != nil {
		d.metrics.volumeErrors.Inc()
		return nil, status.Errorf(codes.Internal, "Could not mount juicefs: %v", err)
	}

	bindSource, err := jfs.CreateVol(ctx, volumeID, volCtx["subPath"])
	if err != nil {
		d.metrics.volumeErrors.Inc()
		return nil, status.Errorf(codes.Internal, "Could not create volume: %s, %v", volumeID, err)
	}

	if err := jfs.BindTarget(ctx, bindSource, target); err != nil {
		d.metrics.volumeErrors.Inc()
		return nil, status.Errorf(codes.Internal, "Could not bind %q at %q: %v", bindSource, target, err)
	}

	if cap, exist := volCtx["capacity"]; exist {
		capacity, err := strconv.ParseInt(cap, 10, 64)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "invalid capacity %s: %v", cap, err)
		}
		if pv != nil {
			capacity = pv.Spec.Capacity.Storage().Value()
		}
		settings, err := d.juicefs.Settings(ctx, volumeID, secrets, volCtx, options)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get settings: %v", err)
		}
		quotaPath := settings.SubPath
		var subdir string
		for _, o := range settings.Options {
			pair := strings.Split(o, "=")
			if len(pair) != 2 {
				continue
			}
			if pair[0] == "subdir" {
				subdir = path.Join("/", pair[1])
			}
		}

		err = d.juicefs.SetQuota(ctx, secrets, settings, path.Join(subdir, quotaPath), capacity)
		if err != nil {
			klog.Error("set quota: ", err)
		}
	}

	klog.V(5).Infof("NodePublishVolume: mounted %s at %s with options %v", volumeID, target, mountOptions)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume is a reverse operation of NodePublishVolume. This RPC is typically called by the CO when the workload using the volume is being moved to a different node, or all the workload using the volume on a node has finished.
func (d *nodeService) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(6).Infof("NodeUnpublishVolume: called with args %+v", req)

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	volumeId := req.GetVolumeId()
	klog.V(5).Infof("NodeUnpublishVolume: volume_id is %s", volumeId)

	err := d.juicefs.JfsUnmount(ctx, volumeId, target)
	if err != nil {
		d.metrics.volumeDelErrors.Inc()
		return nil, status.Errorf(codes.Internal, "Could not unmount %q: %v", target, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetCapabilities response node capabilities to CO
func (d *nodeService) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(6).Infof("NodeGetCapabilities: called with args %+v", req)
	var caps []*csi.NodeServiceCapability
	for _, cap := range nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeGetInfo is called by CO for the node at which it wants to place the workload. The result of this call will be used by CO in ControllerPublishVolume.
func (d *nodeService) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(6).Infof("NodeGetInfo: called with args %+v", req)

	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
	}, nil
}

// NodeExpandVolume unimplemented
func (d *nodeService) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (d *nodeService) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	klog.V(6).Infof("NodeGetVolumeStats: called with args %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	volumePath := req.GetVolumePath()
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume path not provided")
	}

	var exists bool

	err := util.DoWithTimeout(ctx, defaultCheckTimeout, func() (err error) {
		exists, err = mount.PathExists(volumePath)
		return
	})
	if err == nil {
		if !exists {
			klog.V(5).Infof("NodeGetVolumeStats: %s Volume path not exists", volumePath)
			return nil, status.Error(codes.NotFound, "Volume path not exists")
		}
		if d.SafeFormatAndMount.Interface != nil {
			var notMnt bool
			err := util.DoWithTimeout(ctx, defaultCheckTimeout, func() (err error) {
				notMnt, err = mount.IsNotMountPoint(d.SafeFormatAndMount.Interface, volumePath)
				return err
			})
			if err != nil {
				klog.V(5).Infof("NodeGetVolumeStats: Check volume path %s is mountpoint failed: %s", volumePath, err)
				return nil, status.Errorf(codes.Internal, "Check volume path is mountpoint failed: %s", err)
			}
			if notMnt { // target exists but not a mountpoint
				klog.V(5).Infof("NodeGetVolumeStats: %s volume path not mounted", volumePath)
				return nil, status.Error(codes.Internal, "Volume path not mounted")
			}
		}
	} else {
		klog.V(5).Infof("NodeGetVolumeStats: Check volume path %s, err: %s", volumePath, err)
		return nil, status.Errorf(codes.Internal, "Check volume path, err: %s", err)
	}

	totalSize, freeSize, totalInodes, freeInodes := util.GetDiskUsage(volumePath)
	usedSize := int64(totalSize) - int64(freeSize)
	usedInodes := int64(totalInodes) - int64(freeInodes)

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: int64(freeSize),
				Total:     int64(totalSize),
				Used:      usedSize,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: int64(freeInodes),
				Total:     int64(totalInodes),
				Used:      usedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}
