package controller

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/engineapi"
	"github.com/longhorn/longhorn-manager/types"

	"github.com/longhorn/longhorn-instance-manager/api"
	"github.com/longhorn/longhorn-instance-manager/client"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1alpha1"
	lhinformers "github.com/longhorn/longhorn-manager/k8s/pkg/client/informers/externalversions/longhorn/v1alpha1"
)

const (
	defaultManagerPort = ":8500"

	managerProbeInitialDelay  = 1
	managerProbePeriodSeconds = 1

	managerLivenessProbeFailureThreshold  = 60
	managerReadinessProbeFailureThreshold = 15
)

type InstanceManagerController struct {
	namespace    string
	controllerID string

	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	ds *datastore.DataStore

	imStoreSynced cache.InformerSynced
	pStoreSynced  cache.InformerSynced

	queue workqueue.RateLimitingInterface

	watcherLock *sync.Mutex
	watchers    map[string]ManagerWatch
}

type ManagerWatch interface {
	StartWatch()
	StopWatch()
}

type EngineManagerWatch struct {
	done         bool
	engineClient *client.EngineManagerClient
	engineWatch  *api.EngineStream
	imc          *InstanceManagerController
	imName       string
	lock         *sync.Mutex
	stopCh       chan struct{}
}

type ReplicaManagerWatch struct {
	done          bool
	imc           *InstanceManagerController
	imName        string
	imType        types.InstanceManagerType
	lock          *sync.Mutex
	processClient *client.ProcessManagerClient
	processWatch  *api.ProcessStream
	stopCh        chan struct{}
}

func NewInstanceManagerController(
	ds *datastore.DataStore,
	scheme *runtime.Scheme,
	imInformer lhinformers.InstanceManagerInformer,
	pInformer coreinformers.PodInformer,
	kubeClient clientset.Interface,
	namespace, controllerID string) *InstanceManagerController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	imc := &InstanceManagerController{
		namespace:    namespace,
		controllerID: controllerID,

		kubeClient:    kubeClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme, v1.EventSource{Component: "longhorn-instance-manager-controller"}),

		ds: ds,

		imStoreSynced: imInformer.Informer().HasSynced,
		pStoreSynced:  pInformer.Informer().HasSynced,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "longhorn-instance-manager"),

		watcherLock: &sync.Mutex{},
		watchers:    make(map[string]ManagerWatch),
	}

	imInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			im := obj.(*longhorn.InstanceManager)
			imc.enqueueInstanceManager(im)
		},
		UpdateFunc: func(old, cur interface{}) {
			curIM := cur.(*longhorn.InstanceManager)
			imc.enqueueInstanceManager(curIM)
		},
		DeleteFunc: func(obj interface{}) {
			im := obj.(*longhorn.InstanceManager)
			imc.enqueueInstanceManager(im)
		},
	})

	pInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *v1.Pod:
				return imc.filterInstanceManagerPod(t)
			default:
				utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", imc, obj))
				return false
			}
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				imc.enqueueInstanceManagerPod(pod)
			},
			UpdateFunc: func(old, cur interface{}) {
				newPod := cur.(*v1.Pod)
				imc.enqueueInstanceManagerPod(newPod)
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				imc.enqueueInstanceManagerPod(pod)
			},
		},
	})

	return imc
}

func (imc *InstanceManagerController) NewEngineManagerWatch(im *longhorn.InstanceManager) (*EngineManagerWatch, error) {
	if im.Status.IP == "" {
		// IP should be set
		return nil, errors.New("Instance Manager IP was not set before creating watch")
	}

	return &EngineManagerWatch{
		done:         false,
		engineClient: client.NewEngineManagerClient(im.Status.IP + defaultManagerPort),
		imc:          imc,
		imName:       im.Name,
		lock:         &sync.Mutex{},
		stopCh:       make(chan struct{}),
	}, nil
}

func (imc *InstanceManagerController) NewReplicaManagerWatch(im *longhorn.InstanceManager) (*ReplicaManagerWatch, error) {
	if im.Status.IP == "" {
		// IP should be set
		return nil, errors.New("Instance Manager IP was not set before creating watch")
	}

	return &ReplicaManagerWatch{
		done:          false,
		imc:           imc,
		imName:        im.Name,
		lock:          &sync.Mutex{},
		processClient: client.NewProcessManagerClient(im.Status.IP + defaultManagerPort),
		stopCh:        make(chan struct{}),
	}, nil
}

func (imc *InstanceManagerController) filterInstanceManagerPod(obj *v1.Pod) bool {
	isInstanceManager := false
	podContainers := obj.Spec.Containers
	for _, con := range podContainers {
		if con.Name == "engine-manager" || con.Name == "replica-manager" {
			isInstanceManager = true
			break
		}
	}
	return isInstanceManager
}

func (imc *InstanceManagerController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer imc.queue.ShutDown()

	logrus.Infof("Starting Longhorn instance manager controller")
	defer logrus.Infof("Shutting down Longhorn instance manager controller")

	if !controller.WaitForCacheSync("longhorn instance manager", stopCh, imc.imStoreSynced, imc.pStoreSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(imc.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (imc *InstanceManagerController) worker() {
	for imc.processNextWorkItem() {
	}
}

func (imc *InstanceManagerController) processNextWorkItem() bool {
	key, quit := imc.queue.Get()

	if quit {
		return false
	}
	defer imc.queue.Done(key)

	err := imc.syncInstanceManager(key.(string))
	imc.handleErr(err, key)

	return true
}

func (imc *InstanceManagerController) handleErr(err error, key interface{}) {
	if err == nil {
		imc.queue.Forget(key)
		return
	}

	if imc.queue.NumRequeues(key) < maxRetries {
		logrus.Warnf("Error syncing Longhorn instance manager %v: %v", key, err)
		imc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logrus.Warnf("Dropping Longhorn instance manager %v out of the queue: %v", key, err)
	imc.queue.Forget(key)
}

func (imc *InstanceManagerController) syncInstanceManager(key string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "fail to sync instance manager for %v", key)
	}()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if namespace != imc.namespace {
		return nil
	}

	im, err := imc.ds.GetInstanceManager(name)
	if err != nil {
		if datastore.ErrorIsNotFound(err) {
			logrus.Infof("Longhorn instance manager %v has been deleted", key)
			return nil
		}
		return err
	}

	nodeStatusDown := false
	if im.Spec.OwnerID != "" {
		nodeStatusDown, err = imc.ds.IsNodeDownOrDeleted(im.Spec.OwnerID)
		if err != nil {
			logrus.Warnf("Found error while checking if ownerID is down or deleted: %v", err)
		}
	}

	// Node for Instance Manager came back up, take back ownership of Instance Manager.
	if im.Spec.NodeID == imc.controllerID && im.Spec.OwnerID != imc.controllerID {
		im.Spec.OwnerID = imc.controllerID
		newIM, err := imc.ds.UpdateInstanceManager(im)
		if err != nil {
			// Conflict with another controller, keep trying.
			if apierrors.IsConflict(errors.Cause(err)) {
				imc.enqueueInstanceManager(im)
				return nil
			}
			return err
		}
		im = newIM
		logrus.Debugf("Instance Manager Controller %v picked up %v", imc.controllerID, im.Name)
	} else if im.Spec.OwnerID == "" || nodeStatusDown {
		// No owner yet, or Instance Manager's Node is down. Assign to some other Node until the correct Node can take
		// over.
		im.Spec.OwnerID = imc.controllerID
		im, err = imc.ds.UpdateInstanceManager(im)
		if err != nil {
			// we don't mind others coming first
			if apierrors.IsConflict(errors.Cause(err)) {
				return nil
			}
			return err
		}
		logrus.Debugf("Instance Manager Controller %v picked up %v", imc.controllerID, im.Name)
	} else if im.Spec.OwnerID != imc.controllerID {
		// Not ours
		return nil
	}

	if im.DeletionTimestamp != nil {
		if err := imc.cleanupInstanceManager(im); err != nil {
			return err
		}
		return imc.ds.RemoveFinalizerForInstanceManager(im)
	}

	existingIM := im.DeepCopy()
	defer func() {
		if err == nil && !reflect.DeepEqual(existingIM, im) {
			_, err = imc.ds.UpdateInstanceManager(im)
		}
		if apierrors.IsConflict(errors.Cause(err)) {
			logrus.Debugf("Requeue %v due to conflict", key)
			imc.enqueueInstanceManager(im)
			err = nil
		}
	}()

	image, err := imc.ds.GetEngineImage(im.Spec.EngineImage)
	if err != nil {
		if datastore.ErrorIsNotFound(err) {
			logrus.Infof("Engine image %v for instance manager %v has been deleted", im.Spec.EngineImage, key)
			return nil
		}
		return err
	}

	pod, err := imc.ds.GetInstanceManagerPod(im.Name)
	if err != nil {
		return errors.Wrapf(err, "cannot get pod for instance manager %v", im.Name)
	}

	// We're handling the Instance Manager from a Node that isn't supposed to be responsible for it. This could mean
	// that the Node it belongs to went down, or its newly scheduled.
	if im.Spec.NodeID != imc.controllerID {
		im.Status.CurrentState = types.InstanceManagerStateUnknown
		return nil
	}

	if im.Status.CurrentState == types.InstanceManagerStateError {
		if err := imc.cleanupInstanceManager(im); err != nil {
			return err
		}
		if err := imc.createInstanceManagerPod(im, image); err != nil {
			return err
		}
		im.Status.CurrentState = types.InstanceManagerStateStarting
		return nil
	}

	if pod == nil {
		if im.Status.CurrentState != types.InstanceManagerStateStopped {
			im.Status.CurrentState = types.InstanceManagerStateError
			return nil
		}
		if err := imc.createInstanceManagerPod(im, image); err != nil {
			return err
		}
		im.Status.CurrentState = types.InstanceManagerStateStarting
		return nil
	}

	switch pod.Status.Phase {
	case v1.PodPending:
		if im.Status.CurrentState == types.InstanceManagerStateUnknown {
			im.Status.CurrentState = types.InstanceManagerStateStarting
		} else if im.Status.CurrentState != types.InstanceManagerStateStarting {
			im.Status.CurrentState = types.InstanceManagerStateError
			logrus.Errorf("BUG: Instance Manager Pod is pending but doesn't match Instance Manager state")
		}
	case v1.PodRunning:
		// Make sure readiness probe has passed.
		for _, st := range pod.Status.ContainerStatuses {
			if !st.Ready {
				return nil
			}
		}
		switch im.Status.CurrentState {
		case types.InstanceManagerStateRunning:
			imc.watcherLock.Lock()
			// Set up Watcher and add to map if it doesn't exist. If it does, something else has already set the
			// Watcher up.
			if _, ok := imc.watchers[im.Name]; !ok {
				var watch ManagerWatch
				switch im.Spec.Type {
				case types.InstanceManagerTypeEngine:
					watch, err = imc.NewEngineManagerWatch(im)
				case types.InstanceManagerTypeReplica:
					watch, err = imc.NewReplicaManagerWatch(im)
				default:
					imc.watcherLock.Unlock()
					return fmt.Errorf("BUG: instance manager %v has invalid type %v", im.Name, im.Spec.Type)
				}

				if err != nil {
					imc.watcherLock.Unlock()
					return err
				}

				watch.StartWatch()
				imc.watchers[im.Name] = watch
			}
			imc.watcherLock.Unlock()

			if err := imc.pollProcesses(im); err != nil {
				return errors.Wrapf(err, "error running resync of processes for instance manager %v", im.Name)
			}
		case types.InstanceManagerStateStarting:
			fallthrough
		case types.InstanceManagerStateUnknown:
			nodeName := pod.Spec.NodeName
			node, err := imc.kubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			im.Status.CurrentState = types.InstanceManagerStateRunning
			im.Status.IP = pod.Status.PodIP
			im.Status.NodeBootID = node.Status.NodeInfo.BootID
		}
	default:
		im.Status.CurrentState = types.InstanceManagerStateError
	}

	return nil
}

func (imc *InstanceManagerController) pollProcesses(im *longhorn.InstanceManager) error {
	if im.Status.IP == "" {
		// IP should be set
		return errors.New("Instance Manager IP was not set before polling")
	}

	switch im.Spec.Type {
	case types.InstanceManagerTypeEngine:
		engineClient := client.NewEngineManagerClient(im.Status.IP + defaultManagerPort)
		engines, err := engineClient.EngineList()
		if err != nil {
			return err
		}
		// Ignore the entries exists in `engines` but not in `im.Status.Instances`
		for name, instance := range im.Status.Instances {
			newEngineProcess, exist := engines[name]
			if !exist {
				// If `instance.Spec.DeletedAt` is empty, the instance process may be deleted or haven't been started.
				if instance.Spec.DeletedAt != "" {
					delete(im.Status.Instances, name)
				}
				continue
			}
			updateInstancesForInstanceManager(im, engineapi.EngineProcessToInstanceProcess(newEngineProcess), newEngineProcess.Deleted)
		}
	case types.InstanceManagerTypeReplica:
		processClient := client.NewProcessManagerClient(im.Status.IP + defaultManagerPort)
		processes, err := processClient.ProcessList()
		if err != nil {
			return err
		}

		// Ignore the entries exists in `processes` but not in `im.Status.Instances`
		for name, instance := range im.Status.Instances {
			newReplicaProcess, exist := processes[name]
			if !exist {
				// If `instance.Spec.DeletedAt` is empty, the instance process may be deleted or haven't been started.
				if instance.Spec.DeletedAt != "" {
					delete(im.Status.Instances, name)
				}
				continue
			}
			updateInstancesForInstanceManager(im, engineapi.ReplicaProcessToInstanceProcess(newReplicaProcess), newReplicaProcess.Deleted)
		}
	default:
		return fmt.Errorf("BUG: instance manager %v has invalid type %v", im.Name, im.Spec.Type)
	}

	return nil
}

func (imc *InstanceManagerController) enqueueInstanceManager(instanceManager *longhorn.InstanceManager) {
	key, err := controller.KeyFunc(instanceManager)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", instanceManager, err))
		return
	}

	imc.queue.AddRateLimited(key)
}

func (imc *InstanceManagerController) enqueueInstanceManagerPod(pod *v1.Pod) {
	im, err := imc.ds.GetInstanceManager(pod.Name)

	if err != nil {
		if apierrors.IsNotFound(err) {
			logrus.Warnf("Can't find instance manager for pod %v, may be deleted", pod.Name)
			return
		}
		utilruntime.HandleError(fmt.Errorf("couldn't get instance manager: %v", err))
		return
	}
	imc.enqueueInstanceManager(im)
}

// cleanupInstanceManager cleans up the Pod that was created by the Instance Manager and marks any processes that may
// be on that Instance Manager to Error. This is used when we need to recover from an error or prepare for deletion.
func (imc *InstanceManagerController) cleanupInstanceManager(im *longhorn.InstanceManager) error {
	im.Status.IP = ""
	im.Status.NodeBootID = ""

	// Send a signal to the goroutines to stop running. If the channel for the Watch goroutines does not exist,
	// something else already handled it.
	imc.watcherLock.Lock()
	if watch, ok := imc.watchers[im.Name]; ok {
		watch.StopWatch()
		delete(imc.watchers, im.Name)
	}
	imc.watcherLock.Unlock()

	for name, instance := range im.Status.Instances {
		instance.Status.State = types.InstanceStateError
		instance.Status.ErrorMsg = "Instance Manager errored"
		im.Status.Instances[name] = instance
	}

	pod, err := imc.ds.GetInstanceManagerPod(im.Name)
	if err != nil {
		return err
	}
	if pod != nil {
		if err := imc.ds.DeletePod(pod.Name); err != nil {
			return err
		}
	}
	return nil
}

func (imc *InstanceManagerController) createInstanceManagerPod(im *longhorn.InstanceManager, image *longhorn.EngineImage) error {
	var podSpec *v1.Pod
	var err error
	switch im.Spec.Type {
	case types.InstanceManagerTypeEngine:
		podSpec, err = imc.createEngineManagerPodSpec(im, image)
	case types.InstanceManagerTypeReplica:
		podSpec, err = imc.createReplicaManagerPodSpec(im, image)
	}
	if err != nil {
		return err
	}
	pod, err := imc.ds.CreatePod(podSpec)
	if err != nil {
		return errors.Wrapf(err, "failed to create pod for instance manager %v", im.Name)
	}
	logrus.Infof("Created instance manager pod %v for instance manager %v", pod.Name, im.Name)

	return nil
}

func (imc *InstanceManagerController) createGenericManagerPodSpec(im *longhorn.InstanceManager,
	image *longhorn.EngineImage) (*v1.Pod, error) {

	privileged := true
	podSpec := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      im.Name,
			Namespace: imc.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: longhorn.SchemeGroupVersion.String(),
					Kind:       longhorn.SchemeGroupVersion.WithKind("InstanceManager").String(),
					Name:       im.Name,
					UID:        im.UID,
				},
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Image: image.Spec.Image,
					LivenessProbe: &v1.Probe{
						Handler: v1.Handler{
							Exec: &v1.ExecAction{
								Command: []string{"/usr/local/bin/grpc_health_probe", "-addr=:8500"},
							},
						},
						InitialDelaySeconds: managerProbeInitialDelay,
						PeriodSeconds:       managerProbePeriodSeconds,
						FailureThreshold:    managerLivenessProbeFailureThreshold,
					},
					ReadinessProbe: &v1.Probe{
						Handler: v1.Handler{
							Exec: &v1.ExecAction{
								Command: []string{"/usr/local/bin/grpc_health_probe", "-addr=:8500"},
							},
						},
						InitialDelaySeconds: managerProbeInitialDelay,
						PeriodSeconds:       managerProbePeriodSeconds,
						FailureThreshold:    managerReadinessProbeFailureThreshold,
					},
					SecurityContext: &v1.SecurityContext{
						Privileged: &privileged,
					},
				},
			},
			NodeName:      imc.controllerID,
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	// Apply resource requirements to newly created Instance Manager Pods.
	resourceReq, err := GetGuaranteedResourceRequirement(imc.ds)
	if err != nil {
		return nil, err
	}
	if resourceReq != nil {
		podSpec.Spec.Containers[0].Resources = *resourceReq
	}

	return podSpec, nil
}

func (imc *InstanceManagerController) createEngineManagerPodSpec(im *longhorn.InstanceManager,
	image *longhorn.EngineImage) (*v1.Pod, error) {

	podSpec, err := imc.createGenericManagerPodSpec(im, image)
	if err != nil {
		return nil, err
	}

	podSpec.Spec.Containers[0].Name = "engine-manager"
	podSpec.Spec.Containers[0].Command = []string{
		"engine-manager", "daemon", "--listen", "0.0.0.0:8500",
	}
	podSpec.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{
		{
			MountPath: "/host/dev",
			Name:      "dev",
		},
		{
			MountPath: "/host/proc",
			Name:      "proc",
		},
		{
			MountPath: types.EngineBinaryDirectoryInContainer,
			Name:      "engine-binaries",
		},
	}
	podSpec.Spec.Volumes = []v1.Volume{
		{
			Name: "dev",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/dev",
				},
			},
		},
		{
			Name: "proc",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/proc",
				},
			},
		},
		{
			Name: "engine-binaries",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: types.EngineBinaryDirectoryOnHost,
				},
			},
		},
	}
	return podSpec, nil
}

func (imc *InstanceManagerController) createReplicaManagerPodSpec(im *longhorn.InstanceManager,
	image *longhorn.EngineImage) (*v1.Pod, error) {

	podSpec, err := imc.createGenericManagerPodSpec(im, image)
	if err != nil {
		return nil, err
	}

	podSpec.Spec.Containers[0].Name = "replica-manager"
	podSpec.Spec.Containers[0].Command = []string{
		"longhorn-instance-manager", "daemon", "--listen", "0.0.0.0:8500",
	}
	podSpec.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{
		{
			MountPath: "/host",
			Name:      "host",
		},
	}
	podSpec.Spec.Volumes = []v1.Volume{
		{
			Name: "host",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/",
				},
			},
		},
	}
	return podSpec, nil
}

func (w *EngineManagerWatch) StartWatch() {
	// Watch goroutine.
	go func() {
		var err error
		for {
			if w.done {
				break
			}
			w.lock.Lock()
			if w.engineWatch == nil {
				if w.engineWatch, err = w.engineClient.EngineWatch(); err != nil {
					logrus.Errorf("error starting engine watch for instance manager %v: %v", w.imName, err)
					w.lock.Unlock()
					time.Sleep(time.Second)
					continue
				}
			}
			w.lock.Unlock()

			engine, err := w.engineWatch.Recv()
			// Don't need to check for io.EOF, that is still unexpected.
			if err != nil {
				logrus.Errorf("error receiving next item in engine watch: %v", err)
				w.lock.Lock()
				w.engineWatch = nil
				w.lock.Unlock()
				continue
			}

			// Keep retrying until we encounter an error that isn't a conflict or we successfully post the update.
			for {
				im, err := w.imc.ds.GetInstanceManager(w.imName)
				if err != nil {
					logrus.Errorf("could not get instance manager %v: %v", w.imName, err)
					break
				}

				updateInstancesForInstanceManager(im, engineapi.EngineProcessToInstanceProcess(engine), engine.Deleted)

				if _, err = w.imc.ds.UpdateInstanceManager(im); !apierrors.IsConflict(err) {
					if err != nil {
						logrus.Errorf("error updating instance manager %v with engine update %v: %v", im.Name,
							engine.Name, err)
					}
					break
				}
				time.Sleep(time.Second)
			}
		}
	}()

	// Cleanup goroutine.
	go func() {
		<-w.stopCh
		w.done = true

		w.lock.Lock()
		if w.engineWatch != nil {
			if err := w.engineWatch.Close(); err != nil {
				logrus.Errorf("error when closing instance manager %v engine watch: %v", w.imName, err)
			}
		}
		w.lock.Unlock()
	}()
}

func (w *EngineManagerWatch) StopWatch() {
	close(w.stopCh)
}

func (w *ReplicaManagerWatch) StartWatch() {
	// Watch goroutine.
	go func() {
		var err error
		for {
			if w.done {
				break
			}
			w.lock.Lock()
			if w.processWatch == nil {
				if w.processWatch, err = w.processClient.ProcessWatch(); err != nil {
					logrus.Errorf("error starting process watch for instance manager %v: %v", w.imName, err)
					w.lock.Unlock()
					time.Sleep(time.Second)
					continue
				}
			}
			w.lock.Unlock()

			process, err := w.processWatch.Recv()
			// Don't need to check for io.EOF, that is still unexpected.
			if err != nil {
				logrus.Errorf("error receiving next item in process watch: %v", err)
				w.lock.Lock()
				w.processWatch = nil
				w.lock.Unlock()
				continue
			}

			// Keep retrying until we encounter an error that isn't a conflict or we successfully post the update.
			for {
				im, err := w.imc.ds.GetInstanceManager(w.imName)
				if err != nil {
					logrus.Errorf("could not get instance manager %v: %v", w.imName, err)
					break
				}

				updateInstancesForInstanceManager(im, engineapi.ReplicaProcessToInstanceProcess(process), process.Deleted)

				if _, err = w.imc.ds.UpdateInstanceManager(im); !apierrors.IsConflict(err) {
					if err != nil {
						logrus.Errorf("error updating instance manager %v with process update %v: %v", im.Name,
							process.Name, err)
					}
					break
				}
				time.Sleep(time.Second)
			}
		}
	}()

	// Cleanup goroutine.
	go func() {
		<-w.stopCh
		w.done = true

		w.lock.Lock()
		if w.processWatch != nil {
			if err := w.processWatch.Close(); err != nil {
				logrus.Errorf("error when closing instance manager %v process watch: %v", w.imName, err)
			}
		}
		w.lock.Unlock()
	}()
}

func (w *ReplicaManagerWatch) StopWatch() {
	close(w.stopCh)
}

func updateInstancesForInstanceManager(im *longhorn.InstanceManager, newInstance *types.InstanceProcess, deleted bool) {
	name := newInstance.Spec.Name
	currentInstance, exist := im.Status.Instances[name]
	newInstance.Spec.CreatedAt = currentInstance.Spec.CreatedAt
	newInstance.Spec.DeletedAt = currentInstance.Spec.DeletedAt
	if !exist {
		logrus.Warnf("Cannot find instance %v in instance manager %v", name, im.Name)
		return
	}
	// Check UUID to solve the following race condition:
	//     1. Instance manager controller (imc) watch func gets latest result: instance becomes `deleted`  -->  the related entry will be removed
	//     2. Instance handler (ih) creates a new instance with the same name  -->  ih will add entry for instance manager
	//     3. imc poll func gets expired result: instance is `running`  -->  the related volume becomes `attached`
	//     4. imc watch func gets latest result: instance becomes `starting`  -->  the related volume becomes `attaching`
	if newInstance.Spec.UUID != currentInstance.Spec.UUID {
		logrus.Debugf("Instance manager %v will ignore the instance process %v: new instance UUID %v is not the same as existing instance UUID %v",
			im.Name, name, newInstance.Spec.UUID, currentInstance.Spec.UUID)
		return
	}
	if currentInstance.Status.ResourceVersion >= newInstance.Status.ResourceVersion {
		logrus.Debugf("Instance manager %v will ignore expired instance process %v", im.Name, name)
		return
	}

	if deleted {
		// The instance process shouldn't become state `deleted` without the related `DeletedAt` set.
		if currentInstance.Spec.DeletedAt != "" {
			delete(im.Status.Instances, name)
		}
		return
	}

	im.Status.Instances[name] = *newInstance

	return
}
