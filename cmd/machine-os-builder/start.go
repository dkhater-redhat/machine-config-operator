package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/openshift/machine-config-operator/internal/clients"
	"github.com/openshift/machine-config-operator/pkg/controller/build"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	corev1 "k8s.io/api/core/v1"

	"github.com/openshift/machine-config-operator/pkg/version"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var (
	startCmd = &cobra.Command{
		Use:   "start",
		Short: "Starts Machine OS Builder",
		Long:  "",
		Run:   runStartCmd,
	}

	startOpts struct {
		kubeconfig           string
		createDefaults       bool
		copyGlobalPullSecret bool
	}

	errFoo error = fmt.Errorf("configmap not found, will no-op")
)

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.PersistentFlags().StringVar(&startOpts.kubeconfig, "kubeconfig", "", "Kubeconfig file to access a remote cluster (testing only)")
	startCmd.PersistentFlags().BoolVar(&startOpts.createDefaults, "create-defaults", false, "Create default values for machine-os-builder")
	startCmd.PersistentFlags().BoolVar(&startOpts.copyGlobalPullSecret, "copy-global-pull-secret", false, "Copy the global pull secret into the MCO namespace")
}

// Determines which image builder to start based upon the imageBuilderType key
// in the on-cluster-build-config ConfigMap. Defaults to custom-pod-builder.
func getImageBuilderType(cm *corev1.ConfigMap) (string, error) {
	configMapImageBuilder, ok := cm.Data[build.ImageBuilderTypeConfigMapKey]
	if !ok {
		klog.Infof("%s not set, defaulting to %q", build.ImageBuilderTypeConfigMapKey, build.CustomPodImageBuilder)
		return build.CustomPodImageBuilder, nil
	}

	if ok && configMapImageBuilder == "" {
		klog.Infof("%s empty, defaulting to %q", build.ImageBuilderTypeConfigMapKey, build.CustomPodImageBuilder)
		return build.CustomPodImageBuilder, nil
	}

	if ok && configMapImageBuilder != build.OpenshiftImageBuilder && configMapImageBuilder != build.CustomPodImageBuilder {
		return "", fmt.Errorf("invalid %s %q", build.ImageBuilderTypeConfigMapKey, configMapImageBuilder)
	}

	klog.Infof("%s set to %q", build.ImageBuilderTypeConfigMapKey, configMapImageBuilder)
	return configMapImageBuilder, nil
}

// Creates a new BuildController configured for a certain image builder based
// upon the imageBuilderType key in the on-cluster-build-config ConfigMap.
// Defaults to the custom pod builder.
func getController(ctx context.Context, cb *clients.Builder) (*build.Controller, error) {
	onClusterBuildConfigMap, err := getOrCreateBuildControllerConfigMap(ctx, cb)
	if err != nil {
		return nil, err
	}

	imageBuilderType, err := getImageBuilderType(onClusterBuildConfigMap)
	if err != nil {
		return nil, err
	}

	ctrlCtx := ctrlcommon.CreateControllerContext(ctx, cb, componentName)
	buildClients := build.NewClientsFromControllerContext(ctrlCtx)
	cfg := build.DefaultBuildControllerConfig()

	if imageBuilderType == build.OpenshiftImageBuilder {
		return build.NewWithImageBuilder(cfg, buildClients), nil
	}

	return build.NewWithCustomPodBuilder(cfg, buildClients), nil
}

// Starts the controller in a separate Goroutine, but blocks until the supplied
// context is canceled or done.
func startController(ctx context.Context, ctrl *build.Controller) {
	go ctrl.Run(ctx, 5)
	<-ctx.Done()
}

// Blocks the main goroutine so the pod does not exit.
func noop() {
	cmName := fmt.Sprintf("%s/%s", ctrlcommon.MCONamespace, build.OnClusterBuildConfigMapName)
	klog.Infof("ConfigMap %q not found, will no-op (for now)", cmName)
	select {}
}

func runStartCmd(cmd *cobra.Command, args []string) {
	flag.Set("v", "4")
	flag.Set("logtostderr", "true")
	flag.Parse()

	klog.V(2).Infof("Options parsed: %+v", startOpts)

	// To help debugging, immediately log version
	klog.Infof("Version: %+v (%s)", version.Raw, version.Hash)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cb, err := clients.NewBuilder("")
	if err != nil {
		klog.Fatalln(err)
	}

	ctrl, err := getController(ctx, cb)
	if err == nil {
		startController(ctx, ctrl)
		return
	}

	if errors.Is(err, errFoo) {
		noop()
		return
	}

	klog.Fatalln(err)
}
