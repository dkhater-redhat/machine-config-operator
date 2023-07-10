# MachineOSBuilder (On-Cluster Builds)

In the context of the Machine Config Operator (MCO) in Red Hat OpenShift, on-cluster builds refer to the process of building container images directly on the OpenShift cluster, rather than building them outside the cluster (such as on a local machine) and then pushing them to the cluster.

The on-cluster build process is typically performed using OpenShift's build mechanisms, such as BuildConfig and the associated build strategies (Docker, Source-to-Image, Custom, and Pipeline). The on-cluster build process can utilize source code from a variety of sources (like Git), and the built images are stored in OpenShift's integrated registry. The images can then be used to deploy applications on the cluster.

When it comes to the MCO, an on-cluster build might be used to create a custom version of the MCO for use within the cluster. For instance, if you needed to make modifications to the MCO (such as to add new features or to fix bugs), you could use an on-cluster build to create a new container image incorporating your changes. Once the image is built, it can be deployed on the cluster, allowing the nodes in the cluster to utilize the modified version of the MCO.

# Sub-components and design
On-cluster builds are a powerful aspect of the OpenShift ecosystem, allowing administrators to create custom images directly within the cluster itself. Here are the steps outlining the components and their interactions:

1. Apply a new MachineConfig: The process starts when a cluster administrator applies a new MachineConfig. This is a Kubernetes custom resource that the MCO uses to manage system configurations for a cluster's machines.

2. Machine Config Controller (MCC) reacts: The MCC, which is always monitoring for changes in MachineConfigs, processes this new MachineConfig. It takes the raw configurations and combines them with the existing base configuration to create a new rendered MachineConfig. The layered MachineConfigPool will have an `on-cluster-build` label denoting that it serves this purpose. 

3. MachineConfigPool updates: The MCC then updates the relevant MachineConfigPool with this new rendered MachineConfig. A MachineConfigPool represents a group of nodes (machines) in the cluster that should have the same configuration. Upon the identification of an `on-cluster-build` tag, this will trigger an on-cluster-build.

4. Machine-OS-Builder kicks in: On seeing the MachineConfigPool change, the Machine-OS-Builder fetches the rendered MachineConfig and translates it into a form suitable for creating a Dockerfile. This Dockerfile will be used to build a new container image that contains the updated OS and configurations.

5. Image Builder process: The Dockerfile is then used to start an on-cluster build process, typically using OpenShift's own build mechanisms. This results in a new container image.

6. Interchangeable Image Storage: The newly built image is pushed to a container image registry (either the integrated OpenShift registry or an external one). This registry is where the nodes will pull the new OS image from.

7. Machine Config Daemon (MCD) rollout: The Machine-OS-Builder also updates the MachineConfigPool or the individual nodes with an annotation indicating the image pull spec, i.e., the location of the new OS image in the registry. The MCD, which is running on each node, sees this annotation and starts a rollout process. It tells rpm-ostree, the hybrid image/package system that OpenShift uses, to pull the new OS image and apply it.

8. Handling Non-MachineConfig assets: Changes to non-MachineConfig assets (like certificates) don't necessarily trigger a full image build/rollout cycle. Instead, these assets can be updated in-place by the MCD much like the current behavior.

The result of this process is that every node in the MachineConfigPool has its OS and system configurations updated in a consistent and controlled manner, directly from within the cluster itself. 

# Interacting with the MOB/on-cluster builds


# Frequently Asked Questions

