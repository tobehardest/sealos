/*
Copyright 2023 sealos.

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

package controllers

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/labring/sealos/controllers/pkg/utils/env"

	"golang.org/x/sync/semaphore"

	"k8s.io/apimachinery/pkg/selection"

	"k8s.io/apimachinery/pkg/labels"

	userv1 "github.com/labring/sealos/controllers/user/api/v1"

	"github.com/labring/sealos/controllers/user/controllers/helper/config"

	"github.com/minio/minio-go/v7"

	objstorage "github.com/labring/sealos/controllers/pkg/objectstorage"

	"github.com/go-logr/logr"

	"github.com/labring/sealos/controllers/pkg/database"
	"github.com/labring/sealos/controllers/pkg/gpu"
	"github.com/labring/sealos/controllers/pkg/resources"
	"github.com/labring/sealos/controllers/pkg/utils/logger"
	"github.com/labring/sealos/controllers/pkg/utils/retry"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MonitorReconciler reconciles a Monitor object
type MonitorReconciler struct {
	client.Client
	logr.Logger
	Interval              time.Duration
	Scheme                *runtime.Scheme
	stopCh                chan struct{}
	wg                    sync.WaitGroup
	periodicReconcile     time.Duration
	NvidiaGpu             map[string]gpu.NvidiaGPU
	DBClient              database.Interface
	TrafficClient         database.Interface
	Properties            *resources.PropertyTypeLS
	PromURL               string
	ObjStorageClient      *minio.Client
	ObjectStorageInstance string
}

type quantity struct {
	*resource.Quantity
	detail string
}

const (
	PrometheusURL         = "PROM_URL"
	ObjectStorageInstance = "OBJECT_STORAGE_INSTANCE"
	ConcurrentLimit       = "CONCURRENT_LIMIT"
)

var concurrentLimit = int64(DefaultConcurrencyLimit)

const (
	DefaultConcurrencyLimit = 1000
)

//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=resourcequotas/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=infra.sealos.io,resources=infras,verbs=get;list;watch
//+kubebuilder:rbac:groups=infra.sealos.io,resources=infras/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=infra.sealos.io,resources=infras/finalizers,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;list;watch

func NewMonitorReconciler(mgr ctrl.Manager) (*MonitorReconciler, error) {
	r := &MonitorReconciler{
		Client:                mgr.GetClient(),
		Logger:                ctrl.Log.WithName("controllers").WithName("Monitor"),
		stopCh:                make(chan struct{}),
		periodicReconcile:     1 * time.Minute,
		PromURL:               os.Getenv(PrometheusURL),
		ObjectStorageInstance: os.Getenv(ObjectStorageInstance),
	}
	concurrentLimit = env.GetInt64EnvWithDefault(ConcurrentLimit, DefaultConcurrencyLimit)
	var err error
	err = retry.Retry(2, 1*time.Second, func() error {
		r.NvidiaGpu, err = gpu.GetNodeGpuModel(mgr.GetClient())
		if err != nil {
			return fmt.Errorf("failed to get node gpu model: %v", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.Logger.Info("get gpu model", "gpu model", r.NvidiaGpu)
	return r, nil
}

func (r *MonitorReconciler) StartReconciler(ctx context.Context) error {
	r.startPeriodicReconcile()
	if r.TrafficClient != nil {
		r.startMonitorTraffic()
	}
	<-ctx.Done()
	r.stopPeriodicReconcile()
	return nil
}

func (r *MonitorReconciler) startPeriodicReconcile() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		waitNextMinute()
		ticker := time.NewTicker(r.periodicReconcile)
		for {
			select {
			case <-ticker.C:
				r.enqueueNamespacesForReconcile()
			case <-r.stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

func (r *MonitorReconciler) getNamespaceList() (*corev1.NamespaceList, error) {
	namespaceList := &corev1.NamespaceList{}
	req, err := labels.NewRequirement(userv1.UserLabelOwnerKey, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create label requirement: %v", err)
	}
	return namespaceList, r.List(context.Background(), namespaceList, &client.ListOptions{
		LabelSelector: labels.NewSelector().Add(*req),
	})
}

func waitNextMinute() {
	waitTime := time.Until(time.Now().Truncate(time.Minute).Add(1 * time.Minute))
	if waitTime > 0 {
		logger.Info("wait for first reconcile", "waitTime", waitTime)
		time.Sleep(waitTime)
	}
}

func waitNextHour() {
	waitTime := time.Until(time.Now().Truncate(time.Hour).Add(1 * time.Hour))
	if waitTime > 0 {
		logger.Info("wait for first reconcile", "waitTime", waitTime)
		time.Sleep(waitTime)
	}
}

func (r *MonitorReconciler) startMonitorTraffic() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		startTime, endTime := time.Now().UTC(), time.Now().Truncate(time.Hour).Add(1*time.Hour).UTC()
		waitNextHour()
		ticker := time.NewTicker(1 * time.Hour)
		if err := r.MonitorPodTrafficUsed(startTime, endTime); err != nil {
			r.Logger.Error(err, "failed to monitor pod traffic used")
		}
		for {
			select {
			case <-ticker.C:
				startTime, endTime = endTime, endTime.Add(1*time.Hour)
				if err := r.MonitorPodTrafficUsed(startTime, endTime); err != nil {
					r.Logger.Error(err, "failed to monitor pod traffic used")
					break
				}
			case <-r.stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

func (r *MonitorReconciler) stopPeriodicReconcile() {
	close(r.stopCh)
	r.wg.Wait()
}

func (r *MonitorReconciler) enqueueNamespacesForReconcile() {
	r.Logger.Info("enqueue namespaces for reconcile", "time", time.Now().Format(time.RFC3339))

	namespaceList, err := r.getNamespaceList()
	if err != nil {
		r.Logger.Error(err, "failed to list namespaces")
		return
	}

	if err := r.processNamespaceList(namespaceList); err != nil {
		r.Logger.Error(err, "failed to process namespace", "time", time.Now().Format(time.RFC3339))
	}
}

func (r *MonitorReconciler) processNamespaceList(namespaceList *corev1.NamespaceList) error {
	logger.Info("start processNamespaceList", "namespaceList len", len(namespaceList.Items), "time", time.Now().Format(time.RFC3339))
	if len(namespaceList.Items) == 0 {
		r.Logger.Error(fmt.Errorf("no namespace to process"), "")
		return nil
	}
	sem := semaphore.NewWeighted(concurrentLimit)
	wg := sync.WaitGroup{}
	wg.Add(len(namespaceList.Items))
	for i := range namespaceList.Items {
		go func(namespace *corev1.Namespace) {
			defer wg.Done()
			if err := sem.Acquire(context.Background(), 1); err != nil {
				fmt.Printf("Failed to acquire semaphore: %v\n", err)
				return
			}
			defer sem.Release(1)
			if err := r.monitorResourceUsage(namespace); err != nil {
				r.Logger.Error(err, "monitor pod resource", "namespace", namespace.Name)
			}
		}(&namespaceList.Items[i])
	}
	wg.Wait()
	logger.Info("end processNamespaceList", "time", time.Now().Format("2006-01-02 15:04:05"))
	return nil
}

func (r *MonitorReconciler) monitorResourceUsage(namespace *corev1.Namespace) error {
	timeStamp := time.Now().UTC()
	podList := corev1.PodList{}
	resUsed := map[string]map[corev1.ResourceName]*quantity{}
	resNamed := make(map[string]*resources.ResourceNamed)
	if err := r.List(context.Background(), &podList, &client.ListOptions{Namespace: namespace.Name}); err != nil {
		return err
	}
	for _, pod := range podList.Items {
		if pod.Spec.NodeName == "" || (pod.Status.Phase == corev1.PodSucceeded && time.Since(pod.Status.StartTime.Time) > 1*time.Minute) {
			continue
		}
		podResNamed := resources.NewResourceNamed(&pod)
		resNamed[podResNamed.String()] = podResNamed
		if resUsed[podResNamed.String()] == nil {
			resUsed[podResNamed.String()] = initResources()
		}
		// skip pods that do not start for more than 1 minute
		skip := pod.Status.Phase != corev1.PodRunning && (pod.Status.StartTime == nil || time.Since(pod.Status.StartTime.Time) > 1*time.Minute)
		for _, container := range pod.Spec.Containers {
			// gpu only use limit and not ignore pod pending status
			if gpuRequest, ok := container.Resources.Limits[gpu.NvidiaGpuKey]; ok {
				err := r.getGPUResourceUsage(pod, gpuRequest, resUsed[podResNamed.String()])
				if err != nil {
					r.Logger.Error(err, "get gpu resource usage failed", "pod", pod.Name)
				}
			}
			if skip {
				continue
			}
			if cpuRequest, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
				resUsed[podResNamed.String()][corev1.ResourceCPU].Add(cpuRequest)
			} else {
				resUsed[podResNamed.String()][corev1.ResourceCPU].Add(container.Resources.Requests[corev1.ResourceCPU])
			}
			if memoryRequest, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				resUsed[podResNamed.String()][corev1.ResourceMemory].Add(memoryRequest)
			} else {
				resUsed[podResNamed.String()][corev1.ResourceMemory].Add(container.Resources.Requests[corev1.ResourceMemory])
			}
		}
	}

	//logger.Info("mid", "namespace", namespace.Name, "time", timeStamp.Format("2006-01-02 15:04:05"), "resourceMap", resourceMap, "podsRes", podsRes)

	pvcList := corev1.PersistentVolumeClaimList{}
	if err := r.List(context.Background(), &pvcList, &client.ListOptions{Namespace: namespace.Name}); err != nil {
		return fmt.Errorf("failed to list pvc: %v", err)
	}
	for _, pvc := range pvcList.Items {
		if pvc.Status.Phase != corev1.ClaimBound || pvc.Name == resources.KubeBlocksBackUpName {
			continue
		}
		pvcRes := resources.NewResourceNamed(&pvc)
		if resUsed[pvcRes.String()] == nil {
			resNamed[pvcRes.String()] = pvcRes
			resUsed[pvcRes.String()] = initResources()
		}
		resUsed[pvcRes.String()][corev1.ResourceStorage].Add(pvc.Spec.Resources.Requests[corev1.ResourceStorage])
	}
	svcList := corev1.ServiceList{}
	if err := r.List(context.Background(), &svcList, &client.ListOptions{Namespace: namespace.Name}); err != nil {
		return fmt.Errorf("failed to list svc: %v", err)
	}
	for _, svc := range svcList.Items {
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		svcRes := resources.NewResourceNamed(&svc)
		if resUsed[svcRes.String()] == nil {
			resNamed[svcRes.String()] = svcRes
			resUsed[svcRes.String()] = initResources()
		}
		// nodeport 1:1000, the measurement is quantity 1000
		resUsed[svcRes.String()][corev1.ResourceServicesNodePorts].Add(*resource.NewQuantity(1000, resource.BinarySI))
	}

	var monitors []*resources.Monitor

	if username := config.GetUserNameByNamespace(namespace.Name); r.ObjStorageClient != nil {
		if err := r.getObjStorageUsed(username, &resNamed, &resUsed); err != nil {
			r.Logger.Error(err, "failed to get object storage used", "username", username)
		}
	}
	for name, podResource := range resUsed {
		isEmpty, used := r.getResourceUsed(podResource)
		if isEmpty {
			continue
		}
		monitors = append(monitors, &resources.Monitor{
			Category: namespace.Name,
			Used:     used,
			Time:     timeStamp,
			Type:     resNamed[name].Type(),
			Name:     resNamed[name].Name(),
		})
	}
	return r.DBClient.InsertMonitor(context.Background(), monitors...)
}

func (r *MonitorReconciler) getResourceUsed(podResource map[corev1.ResourceName]*quantity) (bool, map[uint8]int64) {
	used := map[uint8]int64{}
	isEmpty := true
	for i := range podResource {
		if podResource[i].MilliValue() == 0 {
			continue
		}
		isEmpty = false
		if pType, ok := r.Properties.StringMap[i.String()]; ok {
			used[pType.Enum] = int64(math.Ceil(float64(podResource[i].MilliValue()) / float64(pType.Unit.MilliValue())))
			continue
		}
		r.Logger.Error(fmt.Errorf("not found resource type"), "resource", i.String())
	}
	return isEmpty, used
}

func (r *MonitorReconciler) getObjStorageUsed(user string, namedMap *map[string]*resources.ResourceNamed, resMap *map[string]map[corev1.ResourceName]*quantity) error {
	buckets, err := objstorage.ListUserObjectStorageBucket(r.ObjStorageClient, user)
	if err != nil {
		return fmt.Errorf("failed to list object storage user %s storage size: %w", user, err)
	}
	if len(buckets) == 0 {
		return nil
	}
	for i := range buckets {
		size, count := objstorage.GetObjectStorageSize(r.ObjStorageClient, buckets[i])
		if count == 0 {
			continue
		}
		bytes, err := objstorage.GetObjectStorageFlow(r.PromURL, buckets[i], r.ObjectStorageInstance)
		if err != nil {
			return fmt.Errorf("failed to get object storage user storage flow: %w", err)
		}
		objStorageNamed := resources.NewObjStorageResourceNamed(buckets[i])
		(*namedMap)[objStorageNamed.String()] = objStorageNamed
		if _, ok := (*resMap)[objStorageNamed.String()]; !ok {
			(*resMap)[objStorageNamed.String()] = initResources()
		}
		(*resMap)[objStorageNamed.String()][corev1.ResourceStorage].Add(*resource.NewQuantity(size, resource.BinarySI))
		(*resMap)[objStorageNamed.String()][resources.ResourceNetwork].Add(*resource.NewQuantity(bytes, resource.BinarySI))
	}
	return nil
}

func (r *MonitorReconciler) MonitorPodTrafficUsed(startTime, endTime time.Time) error {
	namespaceList, err := r.getNamespaceList()
	if err != nil {
		return fmt.Errorf("failed to list namespaces")
	}
	logger.Info("start getPodTrafficUsed", "startTime", startTime.Format(time.RFC3339), "endTime", endTime.Format(time.RFC3339))
	for _, namespace := range namespaceList.Items {
		if err := r.monitorPodTrafficUsed(namespace, startTime, endTime); err != nil {
			r.Logger.Error(err, "failed to monitor pod traffic used", "namespace", namespace.Name)
		}
	}
	return nil
}

func (r *MonitorReconciler) monitorPodTrafficUsed(namespace corev1.Namespace, startTime, endTime time.Time) error {
	monitors, err := r.DBClient.GetDistinctMonitorCombinations(startTime, endTime, namespace.Name)
	if err != nil {
		return fmt.Errorf("failed to get distinct monitor combinations: %w", err)
	}
	for _, monitor := range monitors {
		bytes, err := r.TrafficClient.GetTrafficSentBytes(startTime, endTime, namespace.Name, monitor.Type, monitor.Name)
		if err != nil {
			return fmt.Errorf("failed to get traffic sent bytes: %w", err)
		}
		unit := r.Properties.StringMap[resources.ResourceNetwork].Unit
		used := int64(math.Ceil(float64(resource.NewQuantity(bytes, resource.BinarySI).MilliValue()) / float64(unit.MilliValue())))
		if used == 0 {
			continue
		}
		logger.Info("traffic used ", "monitor", monitor, "used", used, "unit", unit, "bytes", bytes)
		ro := resources.Monitor{
			Category: namespace.Name,
			Name:     monitor.Name,
			Used:     map[uint8]int64{r.Properties.StringMap[resources.ResourceNetwork].Enum: used},
			Time:     endTime.Add(-1 * time.Minute),
			Type:     monitor.Type,
		}
		r.Logger.Info("monitor traffic used", "monitor", ro)
		err = r.DBClient.InsertMonitor(context.Background(), &ro)
		if err != nil {
			return fmt.Errorf("failed to insert monitor: %w", err)
		}
	}
	return nil
}

func (r *MonitorReconciler) getGPUResourceUsage(pod corev1.Pod, gpuReq resource.Quantity, rs map[corev1.ResourceName]*quantity) (err error) {
	nodeName := pod.Spec.NodeName
	gpuModel, exist := r.NvidiaGpu[nodeName]
	if !exist {
		if r.NvidiaGpu, err = gpu.GetNodeGpuModel(r.Client); err != nil {
			return fmt.Errorf("get node gpu model failed: %w", err)
		}
		if gpuModel, exist = r.NvidiaGpu[nodeName]; !exist {
			return fmt.Errorf("node %s not found gpu model", nodeName)
		}
	}
	if _, ok := rs[resources.NewGpuResource(gpuModel.GpuInfo.GpuProduct)]; !ok {
		rs[resources.NewGpuResource(gpuModel.GpuInfo.GpuProduct)] = initGpuResources()
	}
	logger.Info("gpu request", "pod", pod.Name, "namespace", pod.Namespace, "gpu req", gpuReq.String(), "node", nodeName, "gpu model", gpuModel.GpuInfo.GpuProduct)
	rs[resources.NewGpuResource(gpuModel.GpuInfo.GpuProduct)].Add(gpuReq)
	return nil
}

func initResources() (rs map[corev1.ResourceName]*quantity) {
	rs = make(map[corev1.ResourceName]*quantity)
	rs[resources.ResourceGPU] = initGpuResources()
	rs[corev1.ResourceCPU] = &quantity{Quantity: resource.NewQuantity(0, resource.DecimalSI), detail: ""}
	rs[corev1.ResourceMemory] = &quantity{Quantity: resource.NewQuantity(0, resource.BinarySI), detail: ""}
	rs[corev1.ResourceStorage] = &quantity{Quantity: resource.NewQuantity(0, resource.BinarySI), detail: ""}
	rs[resources.ResourceNetwork] = &quantity{Quantity: resource.NewQuantity(0, resource.BinarySI), detail: ""}
	rs[corev1.ResourceServicesNodePorts] = &quantity{Quantity: resource.NewQuantity(0, resource.DecimalSI), detail: ""}
	return
}

func initGpuResources() *quantity {
	return &quantity{Quantity: resource.NewQuantity(0, resource.DecimalSI), detail: ""}
}

func (r *MonitorReconciler) DropMonitorCollectionOlder() error {
	return r.DBClient.DropMonitorCollectionsOlderThan(30)
}
