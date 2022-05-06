/*
 Copyright 2022 Juicedata Inc

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

package apps

import (
	"github.com/juicedata/juicefs-csi-driver/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
)

func PVManage() error {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	// 1. init Manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Port:   9443,
	})
	if err != nil {
		klog.Error("New PV Manager error: %v", err)
		return err
	}
	// 2. init Reconciler（Controller）
	if err := controller.SetupWithPVManager(mgr); err != nil {
		klog.Error("Init PV Reconciler error: %v", err)
		return err
	}

	// 3. start Manager
	go func() {
		if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
			klog.Error("Start PV Manager error: %v", err)
			os.Exit(0)
		}
	}()
	return nil
}
