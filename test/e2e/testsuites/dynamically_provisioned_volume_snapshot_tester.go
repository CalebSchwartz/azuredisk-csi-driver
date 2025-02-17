/*
Copyright 2020 The Kubernetes Authors.

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

package testsuites

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/azuredisk-csi-driver/test/e2e/driver"
	"sigs.k8s.io/azuredisk-csi-driver/test/utils/azure"
	"sigs.k8s.io/azuredisk-csi-driver/test/utils/credentials"

	"github.com/onsi/ginkgo/v2"
	"github.com/pborman/uuid"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclientset "k8s.io/client-go/rest"
	"k8s.io/kubernetes/test/e2e/framework"
)

// DynamicallyProvisionedVolumeSnapshotTest will provision required StorageClass(es),VolumeSnapshotClass(es), PVC(s) and Pod(s)
// Waiting for the PV provisioner to create a new PV
// Testing if the Pod(s) can write and read to mounted volumes
// Create a snapshot, validate the data is still on the disk, and then write and read to it again
// And finally delete the snapshot
// This test only supports a single volume
type DynamicallyProvisionedVolumeSnapshotTest struct {
	CSIDriver                      driver.PVTestDriver
	Pod                            PodDetails
	ShouldOverwrite                bool
	PodOverwrite                   PodDetails
	PodWithSnapshot                PodDetails
	StorageClassParameters         map[string]string
	SnapshotStorageClassParameters map[string]string
	IsWindowsHPCDeployment         bool
}

func (t *DynamicallyProvisionedVolumeSnapshotTest) Run(ctx context.Context, client clientset.Interface, restclient restclientset.Interface, namespace *v1.Namespace) {
	tpod := NewTestPod(client, namespace, t.Pod.Cmd, t.Pod.IsWindows, t.Pod.WinServerVer)
	volume := t.Pod.Volumes[0]
	tpvc, pvcCleanup := volume.SetupDynamicPersistentVolumeClaim(ctx, client, namespace, t.CSIDriver, t.StorageClassParameters)
	for i := range pvcCleanup {
		defer pvcCleanup[i](ctx)
	}
	tpod.SetupVolume(tpvc.persistentVolumeClaim, volume.VolumeMount.NameGenerate+"1", volume.VolumeMount.MountPathGenerate+"1", volume.VolumeMount.ReadOnly)
	ginkgo.By("deploying the pod")
	tpod.Create(ctx)
	defer tpod.Cleanup(ctx)
	ginkgo.By("checking that the pod's command exits with no error")
	tpod.WaitForSuccess(ctx)
	ginkgo.By("sleep 10s to make sure the data is written to the disk")
	time.Sleep(time.Millisecond * 10000)

	ginkgo.By("Checking Prow test resource group")
	creds, err := credentials.CreateAzureCredentialFile()
	framework.ExpectNoError(err, fmt.Sprintf("Error getting creds for AzurePublicCloud %v", err))
	defer func() {
		err := credentials.DeleteAzureCredentialFile()
		framework.ExpectNoError(err)
	}()

	ginkgo.By("Prow test resource group: " + creds.ResourceGroup)

	azureClient, err := azure.GetAzureClient(creds.Cloud, creds.SubscriptionID, creds.AADClientID, creds.TenantID, creds.AADClientSecret)
	framework.ExpectNoError(err)

	//create external resource group
	externalRG := credentials.ResourceGroupPrefix + uuid.NewUUID().String()
	ginkgo.By("Creating external resource group: " + externalRG)
	_, err = azureClient.EnsureResourceGroup(ctx, externalRG, creds.Location, nil)
	framework.ExpectNoError(err)
	defer func() {
		// Only delete resource group the test created
		if strings.HasPrefix(externalRG, credentials.ResourceGroupPrefix) {
			framework.Logf("Deleting resource group %s", externalRG)
			err := azureClient.DeleteResourceGroup(ctx, externalRG)
			framework.ExpectNoError(err)
		}
	}()

	ginkgo.By("creating volume snapshot class with external rg " + externalRG)
	tvsc, cleanup := CreateVolumeSnapshotClass(restclient, namespace, t.SnapshotStorageClassParameters, t.CSIDriver)
	if tvsc.volumeSnapshotClass.Parameters == nil {
		tvsc.volumeSnapshotClass.Parameters = map[string]string{}
	}
	tvsc.volumeSnapshotClass.Parameters["resourceGroup"] = externalRG
	tvsc.Create(ctx)
	defer cleanup()

	ginkgo.By("taking snapshots")
	snapshot := tvsc.CreateSnapshot(ctx, tpvc.persistentVolumeClaim)

	if t.ShouldOverwrite {
		tpod = NewTestPod(client, namespace, t.PodOverwrite.Cmd, t.PodOverwrite.IsWindows, t.Pod.WinServerVer)

		tpod.SetupVolume(tpvc.persistentVolumeClaim, volume.VolumeMount.NameGenerate+"1", volume.VolumeMount.MountPathGenerate+"1", volume.VolumeMount.ReadOnly)
		tpod.SetLabel(TestLabel)
		ginkgo.By("deploying a new pod to overwrite pv data")
		tpod.Create(ctx)
		defer tpod.Cleanup(ctx)
		ginkgo.By("checking that the pod is running")
		tpod.WaitForRunning(ctx)
	}

	defer tvsc.DeleteSnapshot(ctx, snapshot)
	tvsc.ReadyToUse(ctx, snapshot)

	snapshotVolume := volume
	snapshotVolume.DataSource = &DataSource{
		Kind: VolumeSnapshotKind,
		Name: snapshot.Name,
	}
	t.PodWithSnapshot.Volumes = []VolumeDetails{snapshotVolume}
	tPodWithSnapshot, tPodWithSnapshotCleanup := t.PodWithSnapshot.SetupWithDynamicVolumes(ctx, client, namespace, t.CSIDriver, t.StorageClassParameters)
	for i := range tPodWithSnapshotCleanup {
		defer tPodWithSnapshotCleanup[i](ctx)
	}

	if t.ShouldOverwrite && !t.IsWindowsHPCDeployment {
		// 	TODO: add test case which will schedule the original disk and the copied disk on the same node once the conflicting UUID issue is fixed.
		ginkgo.By("Set pod anti-affinity to make sure two pods are scheduled on different nodes")
		tPodWithSnapshot.SetAffinity(&TestPodAntiAffinity)
	}

	ginkgo.By("deploying a pod with a volume restored from the snapshot")
	tPodWithSnapshot.Create(ctx)
	defer tPodWithSnapshot.Cleanup(ctx)
	ginkgo.By("checking that the pod's command exits with no error")
	tPodWithSnapshot.WaitForSuccess(ctx)

}
