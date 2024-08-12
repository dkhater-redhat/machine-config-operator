package build

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containers/image/v5/docker/reference"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	mcfgv1alpha1 "github.com/openshift/api/machineconfiguration/v1alpha1"

	mcfginformersv1alpha1 "github.com/openshift/client-go/machineconfiguration/informers/externalversions/machineconfiguration/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	aggerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	coreclientsetv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	buildinformers "github.com/openshift/client-go/build/informers/externalversions"
	"github.com/openshift/client-go/machineconfiguration/clientset/versioned/scheme"

	buildinformersv1 "github.com/openshift/client-go/build/informers/externalversions/build/v1"

	buildclientset "github.com/openshift/client-go/build/clientset/versioned"

	mcfgclientset "github.com/openshift/client-go/machineconfiguration/clientset/versioned"

	mcfginformers "github.com/openshift/client-go/machineconfiguration/informers/externalversions"

	mcfginformersv1 "github.com/openshift/client-go/machineconfiguration/informers/externalversions/machineconfiguration/v1"
	mcfglistersv1 "github.com/openshift/client-go/machineconfiguration/listers/machineconfiguration/v1"
	mcfglistersv1alpha1 "github.com/openshift/client-go/machineconfiguration/listers/machineconfiguration/v1alpha1"
	corelistersv1 "k8s.io/client-go/listers/core/v1"

	coreinformers "k8s.io/client-go/informers"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"

	"github.com/openshift/machine-config-operator/pkg/apihelpers"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/openshift/machine-config-operator/internal/clients"
)

type ErrInvalidImageBuilder struct {
	Message     string
	InvalidType string
}

func (e *ErrInvalidImageBuilder) Error() string {
	return e.Message
}

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	//nolint:varcheck,deadcode // This will be used eventually
	controllerKind = mcfgv1.SchemeGroupVersion.WithKind("MachineConfigPool")
)

//nolint:revive // If I name this ControllerConfig, that name will be overloaded :P
type BuildControllerConfig struct {
	// updateDelay is a pause to deal with churn in MachineConfigs; see
	// https://github.com/openshift/machine-config-operator/issues/301
	// Default: 5 seconds
	UpdateDelay time.Duration

	// maxRetries is the number of times a machineconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	// Default: 5
	MaxRetries int
}

type ImageBuilder interface {
	Run(context.Context, int)
	StartBuild(ImageBuildRequest) (*corev1.ObjectReference, error)
	IsBuildRunning(*mcfgv1alpha1.MachineOSBuild, *mcfgv1alpha1.MachineOSConfig) (bool, error)
	DeleteBuildObject(*mcfgv1alpha1.MachineOSBuild, *mcfgv1alpha1.MachineOSConfig) error
}

// Controller defines the build controller.
type Controller struct {
	*Clients
	*informers

	eventRecorder record.EventRecorder

	syncHandler func(build string) error

	cmLister              corelistersv1.ConfigMapLister
	ccLister              mcfglistersv1.ControllerConfigLister
	mcpLister             mcfglistersv1.MachineConfigPoolLister
	machineOSBuildLister  mcfglistersv1alpha1.MachineOSBuildLister
	machineOSConfigLister mcfglistersv1alpha1.MachineOSConfigLister

	machineOSConfigListerSynced cache.InformerSynced
	machineOSBuildListerSynced  cache.InformerSynced
	ccListerSynced              cache.InformerSynced
	mcpListerSynced             cache.InformerSynced
	podListerSynced             cache.InformerSynced

	mosQueue workqueue.RateLimitingInterface

	config           BuildControllerConfig
	imageBuilder     ImageBuilder
	imageBuilderType mcfgv1alpha1.MachineOSImageBuilderType
}

// Creates a BuildControllerConfig with sensible production defaults.
func DefaultBuildControllerConfig() BuildControllerConfig {
	return BuildControllerConfig{
		MaxRetries:  5,
		UpdateDelay: time.Second * 5,
	}
}

// Holds each of the clients used by the Build Controller and its subcontrollers.
type Clients struct {
	mcfgclient  mcfgclientset.Interface
	kubeclient  clientset.Interface
	buildclient buildclientset.Interface
}

func NewClientsFromControllerContext(ctrlCtx *ctrlcommon.ControllerContext) *Clients {
	return NewClients(ctrlCtx.ClientBuilder)
}

func NewClients(cb *clients.Builder) *Clients {
	return &Clients{
		mcfgclient:  cb.MachineConfigClientOrDie("machine-os-builder"),
		kubeclient:  cb.KubeClientOrDie("machine-os-builder"),
		buildclient: cb.BuildClientOrDie("machine-os-builder"),
	}
}

// Holds and starts each of the infomrers used by the Build Controller and its subcontrollers.
type informers struct {
	ccInformer              mcfginformersv1.ControllerConfigInformer
	mcpInformer             mcfginformersv1.MachineConfigPoolInformer
	buildInformer           buildinformersv1.BuildInformer
	podInformer             coreinformersv1.PodInformer
	cmInformer              coreinformersv1.ConfigMapInformer
	machineOSBuildInformer  mcfginformersv1alpha1.MachineOSBuildInformer
	machineOSConfigInformer mcfginformersv1alpha1.MachineOSConfigInformer
	toStart                 []interface{ Start(<-chan struct{}) }
}

// Starts the informers, wiring them up to the provided context.
func (i *informers) start(ctx context.Context) {
	for _, startable := range i.toStart {
		startable.Start(ctx.Done())
	}
}

// Creates new informer instances from a given Clients(set).
func newInformers(bcc *Clients) *informers {
	ccInformer := mcfginformers.NewSharedInformerFactory(bcc.mcfgclient, 0)
	mcpInformer := mcfginformers.NewSharedInformerFactory(bcc.mcfgclient, 0)
	cmInformer := coreinformers.NewFilteredSharedInformerFactory(bcc.kubeclient, 0, ctrlcommon.MCONamespace, nil)
	buildInformer := buildinformers.NewSharedInformerFactoryWithOptions(bcc.buildclient, 0, buildinformers.WithNamespace(ctrlcommon.MCONamespace))
	podInformer := coreinformers.NewSharedInformerFactoryWithOptions(bcc.kubeclient, 0, coreinformers.WithNamespace(ctrlcommon.MCONamespace))
	// this may not work, might need a new mcfg client and or a new informer pkg
	machineOSBuildInformer := mcfginformers.NewSharedInformerFactory(bcc.mcfgclient, 0)
	machineOSConfigInformer := mcfginformers.NewSharedInformerFactory(bcc.mcfgclient, 0)

	return &informers{
		ccInformer:              ccInformer.Machineconfiguration().V1().ControllerConfigs(),
		mcpInformer:             mcpInformer.Machineconfiguration().V1().MachineConfigPools(),
		cmInformer:              cmInformer.Core().V1().ConfigMaps(),
		buildInformer:           buildInformer.Build().V1().Builds(),
		podInformer:             podInformer.Core().V1().Pods(),
		machineOSBuildInformer:  machineOSBuildInformer.Machineconfiguration().V1alpha1().MachineOSBuilds(),
		machineOSConfigInformer: machineOSConfigInformer.Machineconfiguration().V1alpha1().MachineOSConfigs(),
		toStart: []interface{ Start(<-chan struct{}) }{
			ccInformer,
			mcpInformer,
			buildInformer,
			cmInformer,
			podInformer,
			machineOSBuildInformer,
			machineOSConfigInformer,
		},
	}
}

// Creates a basic Build Controller instance without configuring an ImageBuilder.
func newBuildController(
	ctrlConfig BuildControllerConfig,
	clients *Clients,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&coreclientsetv1.EventSinkImpl{Interface: clients.kubeclient.CoreV1().Events("")})

	ctrl := &Controller{
		informers:     newInformers(clients),
		Clients:       clients,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineosbuilder-buildcontroller"}),
		mosQueue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineosbuilder"),
		config:        ctrlConfig,
	}

	ctrl.syncHandler = ctrl.syncMachineOSBuilder

	ctrl.ccLister = ctrl.ccInformer.Lister()
	ctrl.mcpLister = ctrl.mcpInformer.Lister()

	ctrl.machineOSConfigLister = ctrl.machineOSConfigInformer.Lister()
	ctrl.machineOSBuildLister = ctrl.machineOSBuildInformer.Lister()

	ctrl.machineOSBuildInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: ctrl.updateMachineOSBuild,
		DeleteFunc: ctrl.deleteMachineOSBuild,
	})

	ctrl.machineOSConfigInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: ctrl.updateMachineOSConfig,
		AddFunc:    ctrl.addMachineOSConfig,
		DeleteFunc: ctrl.deleteMachineOSConfig,
	})

	ctrl.mcpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: ctrl.updateMachineConfigPool,
	})

	ctrl.machineOSConfigListerSynced = ctrl.machineOSConfigInformer.Informer().HasSynced
	ctrl.machineOSBuildListerSynced = ctrl.machineOSBuildInformer.Informer().HasSynced
	ctrl.ccListerSynced = ctrl.ccInformer.Informer().HasSynced
	ctrl.mcpListerSynced = ctrl.mcpInformer.Informer().HasSynced

	return ctrl
}

// Creates a Build Controller instance with a custom pod builder implementation
// for the ImageBuilder.
func NewWithCustomPodBuilder(
	ctrlConfig BuildControllerConfig,
	clients *Clients,
) *Controller {
	ctrl := newBuildController(ctrlConfig, clients)
	ctrl.imageBuilder = newPodBuildController(ctrlConfig, clients, ctrl.customBuildPodUpdater)
	return ctrl
}

// Run executes the render controller.
// TODO: Make this use a context instead of a stop channel.
func (ctrl *Controller) Run(parentCtx context.Context, workers int) {
	klog.Info("Starting MachineOSBuilder-BuildController")
	defer klog.Info("Shutting down MachineOSBuilder-BuildController")

	// Not sure if I actually need a child context here or not.
	ctx, cancel := context.WithCancel(parentCtx)
	defer utilruntime.HandleCrash()
	defer ctrl.mosQueue.ShutDown()
	defer cancel()

	ctrl.informers.start(ctx)

	if !cache.WaitForCacheSync(ctx.Done(), ctrl.mcpListerSynced, ctrl.ccListerSynced) {
		return
	}
	ctrl.monitorBuildPods()

	go ctrl.imageBuilder.Run(ctx, workers)

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.mosWorker, time.Second, ctx.Done())
	}

	<-ctx.Done()
}

func (ctrl *Controller) enqueueMachineOSConfig(mosc *mcfgv1alpha1.MachineOSConfig) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(mosc)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", mosc, err))
		return
	}
	ctrl.mosQueue.Add(key)
}

func (ctrl *Controller) enqueueMachineOSBuild(mosb *mcfgv1alpha1.MachineOSBuild) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(mosb)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", mosb, err))
		return
	}

	ctrl.mosQueue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (ctrl *Controller) mosWorker() {
	for ctrl.processNextMosWorkItem() {
	}
}

func (ctrl *Controller) processNextMosWorkItem() bool {
	key, quit := ctrl.mosQueue.Get()
	if quit {
		return false
	}
	defer ctrl.mosQueue.Done(key)

	err := ctrl.syncHandler(key.(string))
	ctrl.handleErr(err, key)

	return true
}

// Reconciles the MachineConfigPool state with the state of a custom pod object.
func (ctrl *Controller) customBuildPodUpdater(pod *corev1.Pod) error {
	pool, err := ctrl.mcfgclient.MachineconfigurationV1().MachineConfigPools().Get(context.TODO(), pod.Labels[TargetMachineConfigPoolLabelKey], metav1.GetOptions{})
	if err != nil {
		return err
	}

	klog.V(4).Infof("Build pod (%s) is %s", pod.Name, pod.Status.Phase)

	mosc, mosb, err := ctrl.getConfigAndBuildForPool(pool)
	if err != nil {
		return err
	}
	if mosc == nil || mosb == nil {
		return fmt.Errorf("missing MOSC/MOSB for pool %s", pool.Name)
	}

	// We cannot solely rely upon the pod phase to determine whether the build
	// pod is in an error state. This is because it is possible for the build
	// container to enter an error state while the wait-for-done container is
	// still running. The pod phase in this state will still be "Running" as
	// opposed to error.
	if isBuildPodError(pod) {
		if err := ctrl.markBuildFailed(mosc, mosb); err != nil {
			return err
		}

		ctrl.enqueueMachineOSBuild(mosb)
		return nil
	}

	mosbState := ctrlcommon.NewMachineOSBuildState(mosb)
	switch pod.Status.Phase {
	case corev1.PodPending:
		if !mosbState.IsBuildPending() {
			objRef := toObjectRef(pod)
			err = ctrl.markBuildPendingWithObjectRef(mosc, mosb, *objRef)
		}
	case corev1.PodRunning:
		// If we're running, then there's nothing to do right now.
		if !mosbState.IsBuilding() {
			err = ctrl.markBuildInProgress(mosb)
		}
	case corev1.PodSucceeded:
		// If we've succeeded, we need to update the pool to indicate that.
		if !mosbState.IsBuildSuccess() {
			err = ctrl.markBuildSucceeded(mosc, mosb)
		}
	case corev1.PodFailed:
		// If we've failed, we need to update the pool to indicate that.
		if !mosbState.IsBuildFailure() {
			err = ctrl.markBuildFailed(mosc, mosb)
		}
	}

	if err != nil {
		return err
	}

	ctrl.enqueueMachineOSBuild(mosb)
	return nil
}

func (ctrl *Controller) handleConfigMapError(pools []*mcfgv1.MachineConfigPool, err error, key interface{}) {
	klog.V(2).Infof("Error syncing configmap %v: %v", key, err)
	utilruntime.HandleError(err)
	// get mosb assoc. with pool
	for _, pool := range pools {
		klog.V(2).Infof("Dropping machineconfigpool %q out of the queue: %v", pool.Name, err)
		ctrl.mosQueue.Forget(pool.Name)
		ctrl.mosQueue.AddAfter(pool.Name, 1*time.Minute)
	}

}

func (ctrl *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		ctrl.mosQueue.Forget(key)
		return
	}

	if ctrl.mosQueue.NumRequeues(key) < ctrl.config.MaxRetries {
		klog.V(2).Infof("Error syncing machineosbuild %v: %v", key, err)
		ctrl.mosQueue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	klog.V(2).Infof("Dropping machineosbuild %q out of the queue: %v", key, err)
	ctrl.mosQueue.Forget(key)
	ctrl.mosQueue.AddAfter(key, 1*time.Minute)
}

func (ctrl *Controller) syncMachineOSBuilder(key string) error {
	startTime := time.Now()
	klog.V(4).Infof("Started syncing build %q (%v)", key, startTime)
	defer func() {
		klog.V(4).Infof("Finished syncing machineOSBuilder %q (%v)", key, time.Since(startTime))
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	isConfig := false
	var machineOSConfig *mcfgv1alpha1.MachineOSConfig
	machineOSBuild, err := ctrl.machineOSBuildLister.Get(name)
	if k8serrors.IsNotFound(err) {
		// if this is not an existing build. This means our machineOsConfig changed
		isConfig = true
		machineOSConfig, err = ctrl.machineOSConfigLister.Get(name)
		if k8serrors.IsNotFound(err) {
			return nil
		}
	}
	if !isConfig {
		for _, cond := range machineOSBuild.Status.Conditions {
			if cond.Status == metav1.ConditionTrue {
				switch mcfgv1alpha1.BuildProgress(cond.Type) {
				case mcfgv1alpha1.MachineOSBuildPrepared:
					klog.V(4).Infof("Build %s is build prepared and pending", name)
					return nil
				case mcfgv1alpha1.MachineOSBuilding:
					klog.V(4).Infof("Build %s is building", name)
					return nil
				case mcfgv1alpha1.MachineOSBuildFailed:
					klog.V(4).Infof("Build %s is failed", name)
					return nil
				case mcfgv1alpha1.MachineOSBuildInterrupted:
					klog.V(4).Infof("Build %s is interrupted, requeueing", name)
					ctrl.enqueueMachineOSBuild(machineOSBuild)
				case mcfgv1alpha1.MachineOSBuildSucceeded:
					klog.V(4).Infof("Build %s has successfully built", name)
					return nil
				default:
					machineOSConfig, err := ctrl.machineOSConfigLister.Get(machineOSBuild.Spec.MachineOSConfig.Name)
					if err != nil {
						return err
					}
					doABuild, err := shouldWeDoABuild(ctrl.imageBuilder, machineOSConfig, machineOSBuild, machineOSBuild)
					if err != nil {
						return err
					}
					if doABuild {
						ctrl.startBuildForMachineConfigPool(machineOSConfig, machineOSBuild)
					}

				}

			}
		}
	} else {
		// this is a config change or a config CREATION. We need to possibly make a mosb for this build. The updated config is handlded in the updateMachineOSConfig function
		//	if ctrl.imageBuilder.
		var buildExists bool
		var status *mcfgv1alpha1.MachineOSBuildStatus
		machineOSBuild, buildExists = ctrl.doesMOSBExist(machineOSConfig)
		if !buildExists {
			machineOSBuild, status, err = ctrl.CreateBuildFromConfig(machineOSConfig)
			if err != nil {
				return err
			}
			machineOSBuild.Status = *status
			if err := ctrl.startBuildForMachineConfigPool(machineOSConfig, machineOSBuild); err != nil {
				ctrl.syncAvailableStatus(machineOSBuild)
				return err
			}
			return nil
		}
	}
	return ctrl.syncAvailableStatus(machineOSBuild)
}

func (ctrl *Controller) updateMachineConfigPool(old, cur interface{}) {
	oldPool := old.(*mcfgv1.MachineConfigPool).DeepCopy()
	curPool := cur.(*mcfgv1.MachineConfigPool).DeepCopy()
	klog.V(4).Infof("Updating MachineConfigPool %s", oldPool.Name)

	moscOld, mosbOld, err := ctrl.getConfigAndBuildForPool(oldPool)
	if err != nil {
		klog.Errorln(err)
		ctrl.handleErr(err, curPool.Name)
		return
	}
	moscNew, mosbNew, err := ctrl.getConfigAndBuildForPool(curPool)
	if err != nil {
		klog.Errorln(err)
		ctrl.handleErr(err, curPool.Name)
		return
	}

	doABuild := ctrlcommon.BuildDueToPoolChange(oldPool, curPool, moscNew, mosbNew)

	switch {
	// We've transitioned from a layered pool to a non-layered pool.
	case ctrlcommon.IsLayeredPool(moscOld, mosbOld) && !ctrlcommon.IsLayeredPool(moscNew, mosbNew):
		klog.V(4).Infof("MachineConfigPool %s has opted out of layering", curPool.Name)
		if err := ctrl.finalizeOptOut(moscNew, mosbNew); err != nil {
			klog.Errorln(err)
			ctrl.handleErr(err, curPool.Name)
			return
		}
	// We need to do a build.
	case doABuild:
		klog.V(4).Infof("MachineConfigPool %s has changed, requiring a build", curPool.Name)
		var status *mcfgv1alpha1.MachineOSBuildStatus
		mosbNew, status, err = ctrl.CreateBuildFromConfig(moscNew)
		if err != nil {
			klog.Errorln(err)
			ctrl.handleErr(err, curPool.Name)
			return
		}
		mosbNew.Status = *status

		if startErr := ctrl.startBuildForMachineConfigPool(moscNew, mosbNew); startErr != nil {
			syncErr := ctrl.syncAvailableStatus(mosbNew)
			aggErr := aggerrors.NewAggregate([]error{
				syncErr,
				startErr,
			})
			klog.Errorln(aggErr)
			ctrl.handleErr(aggErr, curPool.Name)
			return
		}

	default:
		klog.V(4).Infof("MachineConfigPool %s up-to-date", curPool.Name)
	}
}

func (ctrl *Controller) markBuildInterrupted(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild) error {
	klog.Errorf("Build %s interrupted for pool %s", mosb.Name, mosc.Spec.MachineConfigPool.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {

		bs := ctrlcommon.NewMachineOSBuildState(mosb)
		bs.SetBuildConditions([]metav1.Condition{
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildPrepared),
				Status:  metav1.ConditionFalse,
				Reason:  "Prepared",
				Message: "Build Prepared and Pending",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuilding),
				Status:  metav1.ConditionFalse,
				Reason:  "Running",
				Message: "Image Build In Progress",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildFailed),
				Status:  metav1.ConditionFalse,
				Reason:  "Failed",
				Message: "Build Failed",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildInterrupted),
				Status:  metav1.ConditionTrue,
				Reason:  "Interrupted",
				Message: "Build Interrupted",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildSucceeded),
				Status:  metav1.ConditionFalse,
				Reason:  "Ready",
				Message: "Build Ready",
			},
		})

		// update mosc status
		return ctrl.syncAvailableStatus(bs.Build)
	})

}

// Marks a given MachineConfigPool as a failed build.
func (ctrl *Controller) markBuildFailed(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild) error {
	klog.Errorf("Build %s failed for pool %s", mosb.Name, mosc.Spec.MachineConfigPool.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {

		bs := ctrlcommon.NewMachineOSBuildState(mosb)
		bs.SetBuildConditions([]metav1.Condition{
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildPrepared),
				Status:  metav1.ConditionFalse,
				Reason:  "Prepared",
				Message: "Build Prepared and Pending",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuilding),
				Status:  metav1.ConditionFalse,
				Reason:  "Building",
				Message: "Image Build In Progress",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildFailed),
				Status:  metav1.ConditionTrue,
				Reason:  "Failed",
				Message: "Build Failed",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildInterrupted),
				Status:  metav1.ConditionFalse,
				Reason:  "Interrupted",
				Message: "Build Interrupted",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildSucceeded),
				Status:  metav1.ConditionFalse,
				Reason:  "Ready",
				Message: "Build Ready",
			},
		})

		return ctrl.syncFailingStatus(mosc, bs.Build, fmt.Errorf("BuildFailed"))
	})

}

// Marks a given MachineConfigPool as the build is in progress.
func (ctrl *Controller) markBuildInProgress(mosb *mcfgv1alpha1.MachineOSBuild) error {
	klog.V(4).Infof("Build %s in progress for config %s", mosb.Name, mosb.Spec.DesiredConfig.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {

		bs := ctrlcommon.NewMachineOSBuildState(mosb)

		bs.SetBuildConditions([]metav1.Condition{
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildPrepared),
				Status:  metav1.ConditionFalse,
				Reason:  "Prepared",
				Message: "Build Prepared and Pending",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuilding),
				Status:  metav1.ConditionTrue,
				Reason:  "Building",
				Message: "Image Build In Progress",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildFailed),
				Status:  metav1.ConditionFalse,
				Reason:  "Failed",
				Message: "Build Failed",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildInterrupted),
				Status:  metav1.ConditionFalse,
				Reason:  "Interrupted",
				Message: "Build Interrupted",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildSucceeded),
				Status:  metav1.ConditionFalse,
				Reason:  "Ready",
				Message: "Build Ready",
			},
		})

		return ctrl.syncAvailableStatus(mosb)
	})
}

// Deletes the ephemeral objects we created to perform this specific build.
func (ctrl *Controller) postBuildCleanup(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild, ignoreMissing bool) error {
	// Delete the actual build object itself.
	deleteBuildObject := func() error {
		err := ctrl.imageBuilder.DeleteBuildObject(mosb, mosc)

		if err == nil {
			klog.Infof("Deleted build object %s", newImageBuildRequest(mosc, mosb).getBuildName())
		}

		return err
	}

	// Delete the ConfigMap containing the MachineConfig.
	deleteMCConfigMap := func() error {
		ibr := newImageBuildRequest(mosc, mosb)

		err := ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Delete(context.TODO(), ibr.getMCConfigMapName(), metav1.DeleteOptions{})

		if err == nil {
			klog.Infof("Deleted MachineConfig ConfigMap %s for build %s", ibr.getMCConfigMapName(), ibr.getBuildName())
		}

		return err
	}

	// Delete the ConfigMap containing the rendered Dockerfile.
	deleteDockerfileConfigMap := func() error {
		ibr := newImageBuildRequest(mosc, mosb)

		err := ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Delete(context.TODO(), ibr.getDockerfileConfigMapName(), metav1.DeleteOptions{})

		if err == nil {
			klog.Infof("Deleted Dockerfile ConfigMap %s for build %s", ibr.getDockerfileConfigMapName(), ibr.getBuildName())
		}

		return err
	}

	maybeIgnoreMissing := func(f func() error) func() error {
		return func() error {
			if ignoreMissing {
				return ignoreIsNotFoundErr(f())
			}

			return f()
		}
	}

	// If *any* of these we fail, we want to emit an error. If *all* fail, we
	// want all of the error messages.
	return aggerrors.AggregateGoroutines(
		maybeIgnoreMissing(deleteBuildObject),
		maybeIgnoreMissing(deleteMCConfigMap),
		maybeIgnoreMissing(deleteDockerfileConfigMap),
	)
}

// If one wants to opt out, this removes all of the statuses and object
// references from a given MachineConfigPool.
func (ctrl *Controller) finalizeOptOut(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild) error {
	err := ctrl.postBuildCleanup(mosc, mosb, true)
	return err
}

// Marks a given MachineConfigPool as build successful and cleans up after itself.
func (ctrl *Controller) markBuildSucceeded(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild) error {
	klog.V(4).Infof("Build %s succeeded for MachineConfigPool %s, config %s", mosb.Name, mosc.Spec.MachineConfigPool.Name, mosb.Spec.DesiredConfig.Name)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// REPLACE FINAL PULLSPEC WITH SHA HERE USING ctrl.imagebuilder.FinalPullspec
		ibr := newImageBuildRequest(mosc, mosb)
		digestConfigMap, err := ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Get(context.TODO(), ibr.getDigestConfigMapName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		sha, err := ParseImagePullspec(mosc.Spec.BuildInputs.RenderedImagePushspec, digestConfigMap.Data["digest"])
		if err != nil {
			return fmt.Errorf("could not create digested image pullspec from the pullspec %q and the digest %q: %w", mosc.Status.CurrentImagePullspec, digestConfigMap.Data["digest"], err)
		}

		// now, all we need is to make sure this is used all around. (node controller, getters, etc)
		mosc.Status.CurrentImagePullspec = sha
		mosb.Status.FinalImagePushspec = sha

		if err := ctrl.postBuildCleanup(mosc, mosb, false); err != nil {
			return fmt.Errorf("could not do post-build cleanup: %w", err)
		}

		bs := ctrlcommon.NewMachineOSBuildState(mosb)

		bs.SetBuildConditions([]metav1.Condition{
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildPrepared),
				Status:  metav1.ConditionFalse,
				Reason:  "Prepared",
				Message: "Build Prepared and Pending",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuilding),
				Status:  metav1.ConditionFalse,
				Reason:  "Building",
				Message: "Image Build In Progress",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildFailed),
				Status:  metav1.ConditionFalse,
				Reason:  "Failed",
				Message: "Build Failed",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildInterrupted),
				Status:  metav1.ConditionFalse,
				Reason:  "Interrupted",
				Message: "Build Interrupted",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildSucceeded),
				Status:  metav1.ConditionTrue,
				Reason:  "Ready",
				Message: "Build Ready",
			},
		})

		return ctrl.updateConfigAndBuild(mosc, bs.Build)
	})
}

// Marks a given MachineConfigPool as build pending.
func (ctrl *Controller) markBuildPendingWithObjectRef(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild, objRef corev1.ObjectReference) error {
	klog.V(4).Infof("Build %s for pool %s marked pending with object reference %v", mosb.Name, mosc.Spec.MachineConfigPool.Name, objRef)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		bs := ctrlcommon.NewMachineOSBuildState(mosb)

		bs.SetBuildConditions([]metav1.Condition{
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildPrepared),
				Status:  metav1.ConditionTrue,
				Reason:  "Prepared",
				Message: "Build Prepared and Pending",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuilding),
				Status:  metav1.ConditionFalse,
				Reason:  "Building",
				Message: "Image Build In Progress",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildFailed),
				Status:  metav1.ConditionFalse,
				Reason:  "Failed",
				Message: "Build Failed",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildInterrupted),
				Status:  metav1.ConditionFalse,
				Reason:  "Interrupted",
				Message: "Build Interrupted",
			},
			{
				Type:    string(mcfgv1alpha1.MachineOSBuildSucceeded),
				Status:  metav1.ConditionFalse,
				Reason:  "Ready",
				Message: "Build Ready",
			},
		})

		mcp, err := ctrl.mcfgclient.MachineconfigurationV1().MachineConfigPools().Get(context.TODO(), mosc.Spec.MachineConfigPool.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		mcp.Spec.Configuration.Source = append(mcp.Spec.Configuration.Source, objRef)
		ctrl.mcfgclient.MachineconfigurationV1().MachineConfigPools().Update(context.TODO(), mcp, metav1.UpdateOptions{})
		// add obj ref to mosc

		if bs.Build.Status.BuilderReference == nil {
			mosb.Status.BuilderReference = &mcfgv1alpha1.MachineOSBuilderReference{ImageBuilderType: mosc.Spec.BuildInputs.ImageBuilder.ImageBuilderType, PodImageBuilder: &mcfgv1alpha1.ObjectReference{
				Name:      objRef.Name,
				Group:     objRef.GroupVersionKind().Group,
				Namespace: objRef.Namespace,
				Resource:  objRef.ResourceVersion,
			}}
		}
		return ctrl.syncAvailableStatus(bs.Build)

	})
}

func (ctrl *Controller) updateConfigSpec(mosc *mcfgv1alpha1.MachineOSConfig) error {
	_, err := ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSConfigs().Update(context.TODO(), mosc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("could not update MachineOSConfig %q: %w", mosc.Name, err)
	}
	return nil
}
func (ctrl *Controller) updateConfigAndBuild(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild) error {
	_, err := ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSConfigs().UpdateStatus(context.TODO(), mosc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("could not update MachineOSConfig%q: %w", mosc.Name, err)
	}
	newMosb, err := ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSBuilds().Update(context.TODO(), mosb, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("could not update MachineOSBuild %q: %w", mosb.Name, err)
	}

	newMosb.Status = mosb.Status

	return ctrl.syncAvailableStatus(newMosb)
}

func (ctrl *Controller) getOSImageURLConfig() (*ctrlcommon.OSImageURLConfig, error) {
	cm, err := ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Get(context.TODO(), ctrlcommon.MachineConfigOSImageURLConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not get ConfigMap %s: %w", ctrlcommon.MachineConfigOSImageURLConfigMapName, err)
	}

	return ctrlcommon.ParseOSImageURLConfigMap(cm)
}

func (ctrl *Controller) getImagesConfig() (*ctrlcommon.Images, error) {
	cm, err := ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Get(context.TODO(), ctrlcommon.MachineConfigOperatorImagesConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not get configmap %s: %w", ctrlcommon.MachineConfigOperatorImagesConfigMapName, err)
	}

	return ctrlcommon.ParseImagesFromConfigMap(cm)
}

// Prepares all of the objects needed to perform an image build.
func (ctrl *Controller) prepareForBuild(mosb *mcfgv1alpha1.MachineOSBuild, mosc *mcfgv1alpha1.MachineOSConfig) (ImageBuildRequest, error) {
	ibr := newImageBuildRequestFromBuildInputs(mosb, mosc)

	imagesConfig, err := ctrl.getImagesConfig()
	if err != nil {
		return ibr, fmt.Errorf("could not get images.json config: %w", err)
	}

	ibr.MCOImagePullspec = imagesConfig.MachineConfigOperator

	// populate the "optional" fields, if the user did not specify them
	osImageURLConfig, err := ctrl.getOSImageURLConfig()
	if err != nil {
		return ibr, fmt.Errorf("could not get osImageURL config: %w", err)
	}

	moscNew := mosc.DeepCopy()

	if moscNew.Spec.BuildInputs.BaseOSExtensionsImagePullspec == "" {
		moscNew.Spec.BuildInputs.BaseOSExtensionsImagePullspec = osImageURLConfig.BaseOSExtensionsContainerImage
	}

	if moscNew.Spec.BuildInputs.BaseOSImagePullspec == "" {
		moscNew.Spec.BuildInputs.BaseOSImagePullspec = osImageURLConfig.BaseOSContainerImage
		moscNew.Spec.BuildInputs.ReleaseVersion = osImageURLConfig.ReleaseVersion
	}

	etcPkiEntitlements, err := ctrl.getOptionalSecret(EtcPkiEntitlementSecretName)
	if err != nil {
		return ImageBuildRequest{}, err
	}

	ibr.HasEtcPkiEntitlementKeys = etcPkiEntitlements != nil

	etcPkiRpmGpgKeys, err := ctrl.getOptionalSecret(EtcPkiRpmGpgSecretName)
	if err != nil {
		return ImageBuildRequest{}, err
	}

	ibr.HasEtcPkiRpmGpgKeys = etcPkiRpmGpgKeys != nil

	etcYumReposDConfigs, err := ctrl.getOptionalConfigMap(EtcYumReposDConfigMapName)
	if err != nil {
		return ImageBuildRequest{}, err
	}

	ibr.HasEtcYumReposDConfigs = etcYumReposDConfigs != nil

	// make sure to get these new settings
	ibr.MachineOSConfig = moscNew

	mc, err := ctrl.mcfgclient.MachineconfigurationV1().MachineConfigs().Get(context.TODO(), mosb.Spec.DesiredConfig.Name, metav1.GetOptions{})
	if err != nil {
		return ibr, err
	}

	mcConfigMap, err := ibr.toConfigMap(mc) // ??????
	if err != nil {
		return ImageBuildRequest{}, fmt.Errorf("could not convert MachineConfig %s into ConfigMap: %w", mosb.Spec.DesiredConfig.Name, err) // ????
	}

	_, err = ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Create(context.TODO(), mcConfigMap, metav1.CreateOptions{})
	if err != nil {
		return ImageBuildRequest{}, fmt.Errorf("could not load rendered MachineConfig %s into configmap: %w", mcConfigMap.Name, err)
	}

	klog.Infof("Stored MachineConfig %s in ConfigMap %s for build", mosb.Spec.DesiredConfig.Name, mcConfigMap.Name)

	dockerfileConfigMap, err := ibr.dockerfileToConfigMap()
	if err != nil {
		return ImageBuildRequest{}, fmt.Errorf("could not generate Dockerfile ConfigMap: %w", err)
	}

	_, err = ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Create(context.TODO(), dockerfileConfigMap, metav1.CreateOptions{})
	if err != nil {
		return ImageBuildRequest{}, fmt.Errorf("could not load rendered Dockerfile %s into configmap: %w", dockerfileConfigMap.Name, err)
	}

	klog.Infof("Stored Dockerfile for build %s in ConfigMap %s for build", ibr.getBuildName(), dockerfileConfigMap.Name)

	return ibr, nil
}

// Fetches an optional secret to inject into the build. Returns a nil error if
// the secret is not found.
func (ctrl *Controller) getOptionalSecret(secretName string) (*corev1.Secret, error) {
	optionalSecret, err := ctrl.kubeclient.CoreV1().Secrets(ctrlcommon.MCONamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err == nil {
		klog.Infof("Optional build secret %q found, will include in build", secretName)
		return optionalSecret, nil
	}

	if k8serrors.IsNotFound(err) {
		klog.Infof("Could not find optional secret %q, will not include in build", secretName)
		return nil, nil
	}

	return nil, fmt.Errorf("could not retrieve optional secret: %s: %w", secretName, err)
}

// Fetches an optional ConfigMap to inject into the build. Returns a nil error if
// the ConfigMap is not found.
func (ctrl *Controller) getOptionalConfigMap(configmapName string) (*corev1.ConfigMap, error) {
	optionalConfigMap, err := ctrl.kubeclient.CoreV1().ConfigMaps(ctrlcommon.MCONamespace).Get(context.TODO(), configmapName, metav1.GetOptions{})
	if err == nil {
		klog.Infof("Optional build ConfigMap %q found, will include in build", configmapName)
		return optionalConfigMap, nil
	}

	if k8serrors.IsNotFound(err) {
		klog.Infof("Could not find ConfigMap %q, will not include in build", configmapName)
		return nil, nil
	}

	return nil, fmt.Errorf("could not retrieve optional ConfigMap: %s: %w", configmapName, err)
}

// Determines if we should run a build, then starts a build pod to perform the
// build, and updates the MachineConfigPool with an object reference for the
// build pod.
func (ctrl *Controller) startBuildForMachineConfigPool(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild) error {

	// we need to add osImageURL to mosbuild, will reduce api calls to configmaps
	// ocb config will live in th mosb
	// pool will live in the mosb
	// mc we can get based off the pool specified in the mosb.... though, given how we could use this in two places

	ourConfig, err := ctrl.machineOSConfigLister.Get(mosb.Spec.MachineOSConfig.Name)
	if err != nil {
		return err
	}
	// Replace the user-supplied tag (if present) with the name of the
	// rendered MachineConfig for uniqueness. This will also allow us to
	// eventually do a pre-build registry query to determine if we need to
	// perform a build.
	named, err := reference.ParseNamed(ourConfig.Spec.BuildInputs.RenderedImagePushspec)
	if err != nil {
		return err
	}

	tagged, err := reference.WithTag(named, mosb.Spec.DesiredConfig.Name)
	if err != nil {
		return fmt.Errorf("could not add tag %s to image pullspec %s: %w", mosb.Spec.DesiredConfig.Name, ourConfig.Spec.BuildInputs.RenderedImagePushspec, err)
	}

	ourConfig.Status.CurrentImagePullspec = tagged.String()
	secrets := make(map[string]string)
	secrets["push"] = ourConfig.Spec.BuildInputs.RenderedImagePushSecret.Name
	secrets["pull"] = ourConfig.Spec.BuildInputs.BaseImagePullSecret.Name
	updateMOSC := false
	for key, s := range secrets {
		if s == "" {
			continue
		}
		newS, err := ctrl.validatePullSecret(s)
		if err != nil {
			return err
		}

		if strings.Contains(newS.Name, "canonical") {
			updateMOSC = true
			klog.Infof("Updating build controller config to indicate we have a canonicalized secret %s", newS.Name)
			switch key {
			case "push":
				ourConfig.Spec.BuildInputs.RenderedImagePushSecret.Name = newS.Name
			case "pull":
				ourConfig.Spec.BuildInputs.BaseImagePullSecret.Name = newS.Name
			}
		}
	}

	// ok
	// we need to 1) replace tag

	ibr, err := ctrl.prepareForBuild(mosb, ourConfig)
	if err != nil {
		return fmt.Errorf("could not start build for MachineConfigPool %s: %w", ourConfig.Spec.MachineConfigPool.Name, err)
	}

	objRef, err := ctrl.imageBuilder.StartBuild(ibr)

	if err != nil {
		return err
	}

	err = ctrl.markBuildPendingWithObjectRef(mosc, mosb, *objRef)
	if err != nil {
		return err
	}
	if updateMOSC {
		return ctrl.updateConfigSpec(ourConfig)
	}
	return nil
}

// Ensure that the supplied pull secret exists, is in the correct format, etc.
func (ctrl *Controller) validatePullSecret(name string) (*corev1.Secret, error) {
	secret, err := ctrl.kubeclient.CoreV1().Secrets(ctrlcommon.MCONamespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// If a Docker pull secret lacks the top-level "auths" key, this means that
	// it is a legacy-style pull secret. Buildah does not know how to correctly
	// use one of these secrets. With that in mind, we "canonicalize" it, meaning
	// we inject the existing legacy secret into a {"auths": {}} schema that
	// Buildah can understand. We create a new K8s secret with this info and pass
	// that secret into our image builder instead.
	canonicalized, err := canonicalizePullSecret(secret)
	if err != nil {
		return nil, err
	}

	// If neither the secret from the API nor our canonicalization attempt are
	// identified as canonicalized secrets, that means it does not need to be
	// canonicalized. So we can return it as-is.
	if !isCanonicalizedSecret(canonicalized) && !isCanonicalizedSecret(secret) {
		return secret, nil
	}

	// If the secret from the API server is not canonicalized, but it is
	// identified as canonicalized by our canonicalization attempt, that means
	// the canonicalized secret needs to be created.
	if isCanonicalizedSecret(canonicalized) && !isCanonicalizedSecret(secret) {
		klog.Infof("Found legacy-style secret %s, canonicalizing as %s", secret.Name, canonicalized.Name)
		return ctrl.handleCanonicalizedPullSecret(canonicalized)
	}

	// If we have a canonicalized secret, get the original secret name from the
	// label, then retry validation with the original secret. This will cause the
	// canonicalized secret to be updated if the original secret has changed.
	return ctrl.validatePullSecret(canonicalized.Labels[OriginalSecretNameLabelKey])
}

// Attempt to create a canonicalized pull secret. If the secret already exsits, we should update it.
func (ctrl *Controller) handleCanonicalizedPullSecret(secret *corev1.Secret) (*corev1.Secret, error) {
	if err := validateCanonicalizedSecret(secret); err != nil {
		return nil, fmt.Errorf("could not handle canonicalized secret: %w", err)
	}

	out, err := ctrl.kubeclient.CoreV1().Secrets(ctrlcommon.MCONamespace).Get(context.TODO(), secret.Name, metav1.GetOptions{})
	// We found a canonicalized secret.
	if err == nil {
		// Check if the canonical secret from the API server matches the secret we
		// got. If they match, we're done.
		if bytes.Equal(secret.Data[corev1.DockerConfigJsonKey], out.Data[corev1.DockerConfigJsonKey]) && hasCanonicalizedSecretLabels(secret) {
			klog.Infof("Canonical secret %q up-to-date", secret.Name)
			return out, nil
		}

		// If they do not match, we need to do an update.
		return ctrl.updateCanonicalizedSecret(secret)
	}

	// We don't have a canonical secret, so lets create one.
	if k8serrors.IsNotFound(err) {
		return ctrl.createCanonicalizedSecret(secret)
	}

	return nil, fmt.Errorf("could not get canonicalized secret %q: %w", secret.Name, err)
}

// Creates a canonicalized secret.
func (ctrl *Controller) createCanonicalizedSecret(secret *corev1.Secret) (*corev1.Secret, error) {
	if err := validateCanonicalizedSecret(secret); err != nil {
		return nil, fmt.Errorf("could not create canonicalized secret: %w", err)
	}

	out, err := ctrl.kubeclient.CoreV1().Secrets(ctrlcommon.MCONamespace).Create(context.TODO(), secret, metav1.CreateOptions{})

	// Secret creation was successful.
	if err == nil {
		klog.Infof("Created canonicalized secret %s", secret.Name)
		return out, nil
	}

	// It is possible that BuildController could attempt to create the same
	// canonicalized secret more than once if two (or more) MachineOSConfigs are
	// created at the same time that specify the same secret which must be
	// canonicalized.
	//
	// If this is the case, all subsequent secret creation attempts (after the
	// first sucesssful attempt) will fail. In this situation, the best and
	// simplest thing to do is to call ctrl.validatePullSecret() with the
	// original secret name. This is because it is possible that the secret could
	// have been updated and we should handle that more gracefully.
	if k8serrors.IsAlreadyExists(err) {
		klog.Infof("Canonicalized secret %s already exists", secret.Name)
		return ctrl.validatePullSecret(secret.Labels[OriginalSecretNameLabelKey])
	}

	return nil, fmt.Errorf("could not create canonicalized secret %q: %w", secret.Name, err)
}

// Performs the update operation for updating the canonicalized secret. This is
// wrapped in a retry.RetryOnConflict() function because it is possible that
// BuildController might try to update this secret multiple times in rapid
// succession.
func (ctrl *Controller) updateCanonicalizedSecret(secret *corev1.Secret) (*corev1.Secret, error) {
	if err := validateCanonicalizedSecret(secret); err != nil {
		return nil, fmt.Errorf("could not update canonicalized secret: %w", err)
	}

	var out *corev1.Secret

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		found, err := ctrl.kubeclient.CoreV1().Secrets(ctrlcommon.MCONamespace).Get(context.TODO(), secret.Name, metav1.GetOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("could not get canonical secret %q: %w", secret.Name, err)
		}

		found.Data = secret.Data
		updated, err := ctrl.kubeclient.CoreV1().Secrets(ctrlcommon.MCONamespace).Update(context.TODO(), found, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("could not update canonical secret %q: %w", secret.Name, err)
		}

		out = updated

		klog.Infof("Updated canonical secret %s", secret.Name)

		return nil
	})

	return out, err
}

func (ctrl *Controller) addMachineOSConfig(cur interface{}) {
	m := cur.(*mcfgv1alpha1.MachineOSConfig).DeepCopy()
	ctrl.enqueueMachineOSConfig(m)
	klog.V(4).Infof("Adding MachineOSConfig %s", m.Name)

}

func (ctrl *Controller) updateMachineOSConfig(old, cur interface{}) {
	oldMOSC := old.(*mcfgv1alpha1.MachineOSConfig).DeepCopy()
	curMOSC := cur.(*mcfgv1alpha1.MachineOSConfig).DeepCopy()

	if equality.Semantic.DeepEqual(oldMOSC.Spec.BuildInputs, curMOSC.Spec.BuildInputs) {
		// we do not want to trigger an update func just for MOSC status, we dont act on the status
		return
	}

	klog.Infof("Updating MachineOSConfig %s", oldMOSC.Name)

	doABuild := configChangeCauseBuild(oldMOSC, curMOSC)
	if doABuild {
		build, exists := ctrl.doesMOSBExist(curMOSC)
		if exists {
			ctrl.startBuildForMachineConfigPool(curMOSC, build) // ?
		}
		// if the mosb does not exist, lets just enqueue the mosc and let the sync handler take care of the new object creation
	}
	ctrl.enqueueMachineOSConfig(curMOSC)
}

func (ctrl *Controller) deleteMachineOSConfig(cur interface{}) {
	mosc, ok := cur.(*mcfgv1alpha1.MachineOSConfig)
	mcp, err := ctrl.mcpLister.Get(mosc.Spec.MachineConfigPool.Name)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("MachineOSConfig's MachineConfigPool cannot be found"))
		return
	}
	// first, we need to stop and delete any existing builds.
	mosb, err := ctrl.machineOSBuildLister.Get(fmt.Sprintf("%s-%s-builder", mosc.Spec.MachineConfigPool.Name, mcp.Spec.Configuration.Name))
	if err == nil {
		if running, _ := ctrl.imageBuilder.IsBuildRunning(mosb, mosc); running {
			// we need to stop the build.
			ctrl.imageBuilder.DeleteBuildObject(mosb, mosc)
			ctrl.markBuildInterrupted(mosc, mosb)
		}
		ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSBuilds().Delete(context.TODO(), mosb.Name, metav1.DeleteOptions{})
	}
	if !ok {
		tombstone, ok := cur.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", cur))
			return
		}
		mosc, ok = tombstone.Obj.(*mcfgv1alpha1.MachineOSConfig)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a MachineOSConfig %#v", cur))
			return
		}
	}
	klog.V(4).Infof("Deleting MachineOSConfig %s", mosc.Name)
}

func (ctrl *Controller) updateMachineOSBuild(old, cur interface{}) {
	oldMOSB := old.(*mcfgv1alpha1.MachineOSBuild).DeepCopy()
	curMOSB := cur.(*mcfgv1alpha1.MachineOSBuild).DeepCopy()

	if equality.Semantic.DeepEqual(oldMOSB.Status, oldMOSB.Status) {
		// we do not want to trigger an update func just for MOSB spec, we dont act on the spec
		return
	}

	klog.Infof("Updating MachineOSBuild %s", oldMOSB.Name)
	ourConfig, err := ctrl.machineOSConfigLister.Get(curMOSB.Spec.MachineOSConfig.Name)
	if err != nil {
		return
	}

	doABuild, err := shouldWeDoABuild(ctrl.imageBuilder, ourConfig, oldMOSB, curMOSB)
	if err != nil {
		return
	}
	if doABuild {
		ctrl.startBuildForMachineConfigPool(ourConfig, curMOSB)
	}
	ctrl.enqueueMachineOSBuild(curMOSB)
}

func (ctrl *Controller) deleteMachineOSBuild(mosb interface{}) {
	m, ok := mosb.(*mcfgv1alpha1.MachineOSBuild)
	if !ok {
		tombstone, ok := mosb.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", mosb))
			return
		}
		m, ok = tombstone.Obj.(*mcfgv1alpha1.MachineOSBuild)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a MachineOSBuild %#v", mosb))
			return
		}
	}
	klog.V(4).Infof("Deleting MachineOSBuild %s", m.Name)
}

func (ctrl *Controller) syncAvailableStatus(mosb *mcfgv1alpha1.MachineOSBuild) error {
	// I'm not sure what the consequences are of not doing this.
	//nolint:gocritic // Leaving this here for review purposes.

	sdegraded := apihelpers.NewMachineOSBuildCondition(string(mcfgv1alpha1.MachineOSBuildFailed), metav1.ConditionFalse, "MOSCAvailable", "MOSC")
	apihelpers.SetMachineOSBuildCondition(&mosb.Status, *sdegraded)

	if _, err := ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSBuilds().UpdateStatus(context.TODO(), mosb, metav1.UpdateOptions{}); err != nil {
		return err
	}

	return nil
}

func (ctrl *Controller) syncFailingStatus(mosc *mcfgv1alpha1.MachineOSConfig, mosb *mcfgv1alpha1.MachineOSBuild, err error) error {
	sdegraded := apihelpers.NewMachineOSBuildCondition(string(mcfgv1alpha1.MachineOSBuildFailed), metav1.ConditionTrue, "BuildFailed", fmt.Sprintf("Failed to build configuration for pool %s: %v", mosc.Spec.MachineConfigPool.Name, err))
	apihelpers.SetMachineOSBuildCondition(&mosb.Status, *sdegraded)
	if _, updateErr := ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSBuilds().UpdateStatus(context.TODO(), mosb, metav1.UpdateOptions{}); updateErr != nil {
		klog.Errorf("Error updating MachineOSBuild %s: %v", mosb.Name, updateErr)
	}
	return err
}

func configChangeCauseBuild(old, cur *mcfgv1alpha1.MachineOSConfig) bool {
	return equality.Semantic.DeepEqual(old.Spec.BuildInputs, cur.Spec.BuildInputs)
}

// Determines if we should do a build based upon the state of our
// MachineConfigPool, the presence of a build pod, etc.
func shouldWeDoABuild(builder interface {
	IsBuildRunning(*mcfgv1alpha1.MachineOSBuild, *mcfgv1alpha1.MachineOSConfig) (bool, error)
}, mosc *mcfgv1alpha1.MachineOSConfig, oldMOSB, curMOSB *mcfgv1alpha1.MachineOSBuild) (bool, error) {
	// get desired and current. If desired != current,
	// assume we are doing a build. remove the whole layered pool annotation workflow

	if oldMOSB.Spec.DesiredConfig != curMOSB.Spec.DesiredConfig {
		// the desiredConfig changed. We need to do an update
		// but check that there isn't already a build.
		// If a build is found running, we should not do a build.
		isRunning, err := builder.IsBuildRunning(curMOSB, mosc)

		return !isRunning, err

		// check for image pull sped changing?
	}
	return false, nil
}

// Determines if an object is an ephemeral build object by examining its labels.
func isEphemeralBuildObject(obj metav1.Object) bool {
	return EphemeralBuildObjectSelector().Matches(labels.Set(obj.GetLabels()))
}

// Determines if an object is managed by this controller by examining its labels.
func hasAllRequiredOSBuildLabels(inLabels map[string]string) bool {
	return OSBuildSelector().Matches(labels.Set(inLabels))
}

func (ctrl *Controller) doesMOSBExist(mosc *mcfgv1alpha1.MachineOSConfig) (*mcfgv1alpha1.MachineOSBuild, bool) {
	mcp, err := ctrl.mcpLister.Get(mosc.Spec.MachineConfigPool.Name)
	if err != nil {
		return nil, false
	}
	mosb, err := ctrl.machineOSBuildLister.Get(fmt.Sprintf("%s-%s-builder", mosc.Spec.MachineConfigPool.Name, mcp.Spec.Configuration.Name))
	if err != nil && k8serrors.IsNotFound(err) {
		return nil, false
	} else if mosb != nil {
		return mosb, true
	}
	return nil, false
}

func (ctrl *Controller) CreateBuildFromConfig(config *mcfgv1alpha1.MachineOSConfig) (*mcfgv1alpha1.MachineOSBuild, *mcfgv1alpha1.MachineOSBuildStatus, error) {
	mcp, err := ctrl.mcpLister.Get(config.Spec.MachineConfigPool.Name)
	if err != nil {
		return nil, nil, err
	}
	now := metav1.Now()
	build := mcfgv1alpha1.MachineOSBuild{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MachineOSBuild",
			APIVersion: "machineconfiguration.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s-builder", config.Spec.MachineConfigPool.Name, mcp.Spec.Configuration.Name),
		},
		Spec: mcfgv1alpha1.MachineOSBuildSpec{
			RenderedImagePushspec: config.Spec.BuildInputs.RenderedImagePushspec,
			Version:               1,
			ConfigGeneration:      1,
			DesiredConfig: mcfgv1alpha1.RenderedMachineConfigReference{
				Name: mcp.Spec.Configuration.Name,
			},
			MachineOSConfig: mcfgv1alpha1.MachineOSConfigReference{
				Name: config.Name,
			},
		},
		Status: mcfgv1alpha1.MachineOSBuildStatus{
			BuildStart: &now,
		},
	}
	mosb, err := ctrl.mcfgclient.MachineconfigurationV1alpha1().MachineOSBuilds().Create(context.TODO(), &build, metav1.CreateOptions{})
	return mosb, &build.Status, err
}

func (ctrl *Controller) getConfigAndBuildForPool(pool *mcfgv1.MachineConfigPool) (*mcfgv1alpha1.MachineOSConfig, *mcfgv1alpha1.MachineOSBuild, error) {
	moscs, err := ctrl.machineOSConfigLister.List(labels.Everything())
	if err != nil {
		return nil, nil, err
	}

	mosbs, err := ctrl.machineOSBuildLister.List(labels.Everything())
	if err != nil {
		return nil, nil, err
	}

	var mosb *mcfgv1alpha1.MachineOSBuild
	var mosc *mcfgv1alpha1.MachineOSConfig

	for _, config := range moscs {
		if config.Spec.MachineConfigPool.Name == pool.Name {
			mosc = config
			break
		}
	}

	if mosc == nil {
		return nil, nil, nil
	}

	for _, build := range mosbs {
		if build.Spec.MachineOSConfig.Name == mosc.Name {
			if build.Spec.DesiredConfig.Name == pool.Spec.Configuration.Name {
				mosb = build
				break
			}
		}
	}

	return mosc, mosb, nil
}

// Determines if the build pod is in an error state by examining the individual
// container statuses. Returns true if a single container is in an error state.
func isBuildPodError(pod *corev1.Pod) bool {
	errStates := map[string]struct{}{
		"ErrImagePull":         {},
		"CreateContainerError": {},
	}

	for _, container := range pod.Status.ContainerStatuses {
		if container.State.Waiting != nil {
			if _, ok := errStates[container.State.Waiting.Reason]; ok {
				return true
			}
		}

		if container.State.Terminated != nil && container.State.Terminated.ExitCode != 0 {
			return true
		}
	}

	return false
}

func (ctrl *Controller) monitorBuildPods() {
	ctrl.podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*corev1.Pod)

			if ctrl.isBuildPod(newPod) {
				ctrl.handleBuildPodUpdate(newPod)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if ok && ctrl.isBuildPod(pod) {
				ctrl.handleBuildPodDeletion(pod)
			}
		},
	})
}

func (ctrl *Controller) isBuildPod(pod *corev1.Pod) bool {
	return pod.Labels["build.openshift.io/build"] == "true"
}

func (ctrl *Controller) logPodEvent(pod *corev1.Pod, message string) {
	klog.Infof("Pod %s/%s: %s", pod.Namespace, pod.Name, message)
}

func (ctrl *Controller) handleBuildPodUpdate(pod *corev1.Pod) {
	if pod.Status.Phase == corev1.PodFailed && ctrl.isPodEvicted(pod) {
		ctrl.logPodEvent(pod, "Build pod evicted. Rescheduling build.")
		ctrl.rescheduleBuild(pod)
	} else if pod.Status.Phase == corev1.PodFailed {
		ctrl.markBuildFailedDueToError(pod)
	}
}

func (ctrl *Controller) isPodEvicted(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Reason == "Evicted" {
			return true
		}
	}
	return false
}

func (ctrl *Controller) handleBuildPodDeletion(pod *corev1.Pod) {
	if ctrl.isPodEvicted(pod) {
		ctrl.logPodEvent(pod, "Build pod evicted due to node drain. Rescheduling build.")
		ctrl.rescheduleBuild(pod)
	}
}
func (ctrl *Controller) markBuildFailedDueToError(pod *corev1.Pod) {
	pool, err := ctrl.mcfgclient.MachineconfigurationV1().MachineConfigPools().Get(context.TODO(), pod.Labels[TargetMachineConfigPoolLabelKey], metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get MachineConfigPool for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}

	mosc, mosb, err := ctrl.getConfigAndBuildForPool(pool)
	if err != nil {
		klog.Errorf("No MachineOSBuild found for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}
	ctrl.markBuildFailed(mosc, mosb)
}

func (ctrl *Controller) rescheduleBuild(pod *corev1.Pod) {
	pool, err := ctrl.mcfgclient.MachineconfigurationV1().MachineConfigPools().Get(context.TODO(), pod.Labels[TargetMachineConfigPoolLabelKey], metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get MachineConfigPool for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}

	mosc, mosb, err := ctrl.getConfigAndBuildForPool(pool)
	if err != nil {
		klog.Errorf("No MachineOSBuild found for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}

	if err := ctrl.startBuildForMachineConfigPool(mosc, mosb); err != nil {
		klog.Errorf("Failed to reschedule build %s: %v", mosb.Name, err)
	}
}
