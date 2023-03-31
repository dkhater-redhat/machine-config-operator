package e2e_test

import (
	"context"
	"testing"

	"github.com/openshift/machine-config-operator/test/framework"
	"github.com/openshift/machine-config-operator/test/helpers"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOnClusterBuildLabel(t *testing.T) {
	cs := framework.NewClientSet("")
	ctx := context.TODO()

	// Create and label a MachineConfigPool
	deleteMCP := helpers.CreateMCP(t, cs, "on-cluster-build")

	helpers.LabelRandomNodeFromPool(t, cs, "worker", "node-role.kubernetes.io/on-cluster-build")
	helpers.MCPNameToRole("on-cluster-build")
	node := helpers.GetSingleNodeByRole(t, cs, "on-cluster-build")

	unlabelFunc := helpers.LabelNode(t, cs, node, "node-role.kubernetes.io/on-cluster-build")

	// Create a MachineConfig
	onClusterBuildConfig := helpers.CreateMC("on-cluster-build-config", "on-cluster-build")

	t.Cleanup(func() {
		unlabelFunc()

		// Delete the MachineConfig
		require.NoError(t, cs.MachineConfigs().Delete(ctx, onClusterBuildConfig.Name, metav1.DeleteOptions{}), "Failed to delete MachineConfig")

		deleteMCP()
	})

	// Create MachineConfig
	_, err := cs.MachineConfigs().Create(ctx, onClusterBuildConfig, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create MachineConfig")

	// Verify if the node has the "on-cluster-build" label
	hasLabel := helpers.HasLabel(node, "node-role.kubernetes.io/on-cluster-build")
	require.True(t, hasLabel, "Node does not have the expected label")
}
