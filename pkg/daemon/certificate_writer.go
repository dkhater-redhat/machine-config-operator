package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ghodss/yaml"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	"github.com/openshift/machine-config-operator/pkg/daemon/constants"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/klog/v2"
)

var ccRequeueDelay = 1 * time.Minute

func (dn *Daemon) handleControllerConfigEvent(obj interface{}) {
	controllerConfig := obj.(*mcfgv1.ControllerConfig)
	klog.V(4).Infof("Updating ControllerConfig %s", controllerConfig.Name)
	dn.enqueueControllerConfig(controllerConfig)
}

func (dn *Daemon) enqueueControllerConfig(controllerConfig *mcfgv1.ControllerConfig) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(controllerConfig)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %w", controllerConfig, err))
		return
	}
	dn.ccQueue.AddRateLimited(key)
}

func (dn *Daemon) enqueueControllerConfigAfter(controllerConfig *mcfgv1.ControllerConfig, after time.Duration) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(controllerConfig)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %w", controllerConfig, err))
		return
	}

	dn.ccQueue.AddAfter(key, after)
}

func (dn *Daemon) controllerConfigWorker() {
	for dn.processNextControllerConfigWorkItem() {
	}
}

func (dn *Daemon) processNextControllerConfigWorkItem() bool {
	key, quit := dn.ccQueue.Get()
	if quit {
		return false
	}
	defer dn.ccQueue.Done(key)

	err := dn.syncControllerConfigHandler(key)
	dn.handleControllerConfigErr(err, key)

	return true
}

func (dn *Daemon) handleControllerConfigErr(err error, key string) {
	if err == nil {
		dn.ccQueue.Forget(key)
		return
	}

	if err := dn.updateErrorState(err); err != nil {
		klog.Errorf("Could not update annotation: %v", err)
	}
	// This is at V(2) since the updateErrorState() call above ends up logging too
	klog.V(2).Infof("Error syncing ControllerConfig %v (retries %d): %v", key, dn.ccQueue.NumRequeues(key), err)
	dn.ccQueue.AddRateLimited(key)
}

// nolint:gocyclo
// syncControllerConfigHandler processes updates to the ControllerConfig resource.
//
// ## What is ControllerConfig?
// ControllerConfig is a cluster-scoped CR that the Machine Config Operator (MCO) uses
// to distribute configuration data to Machine Config Daemons (MCDs) running on each node.
// It contains:
// - CA certificates for the API server (KubeAPIServerServingCAData)
// - Cloud provider CA certificates
// - Image registry pull secrets
// - Image registry CA bundles
// - Annotations for triggering actions (like ServiceCARotateAnnotation)
//
// ## How Certificate Rotation Works:
// 1. OpenShift has multiple CA certificates that sign server certificates:
//    - kube-apiserver-lb-signer: signs the loadbalancer cert (api.cluster.com, api-int.cluster.com)
//    - kube-apiserver-localhost-signer: signs localhost recovery certs
//    - service-ca: signs service certificates
//
// 2. These certificates can expire or be rotated for security reasons.
//
// 3. When a CA cert rotates (e.g., loadbalancer-serving-signer expires):
//    a. cluster-kube-apiserver-operator detects the expiry
//    b. Generates a NEW CA certificate
//    c. Updates the kubeconfig-data ConfigMap with BOTH old and new CA certs
//    d. Sets ServiceCARotateAnnotation="true" on the ControllerConfig
//    e. Copies the updated CA bundle to ControllerConfig.Spec.KubeAPIServerServingCAData
//
// 4. MCDs watch the ControllerConfig and detect the annotation change
//
// 5. MCDs must update two kubeconfigs on each node:
//    - /etc/kubernetes/kubeconfig (MCD's own kubeconfig)
//    - /var/lib/kubelet/kubeconfig (kubelet's kubeconfig)
//
// 6. After updating kubeconfigs, MCD must:
//    - Restart kubelet (so it reloads the new CA bundle)
//    - EXIT ITSELF (so kubelet restarts it with fresh CA certs)
//
// 7. The MCD's Kubernetes client (dn.kubeClient, dn.nodeWriter) is initialized at
//    pod startup with the CA bundle from /etc/kubernetes/kubeconfig. This client
//    CANNOT be reloaded in-memory - the only way to refresh it is to restart the pod.
//
// ## The Bug This Function Fixes:
// Before this fix, MCDs would defer loadbalancer-serving-signer rotations, meaning they
// would update the kubeconfig but NOT restart kubelet or exit. This caused MCDs to
// continue running with stale CA bundles in their Kubernetes clients, leading to x509
// errors when trying to talk to the API server after the old cert expired.
//
func (dn *Daemon) syncControllerConfigHandler(key string) error {
	startTime := time.Now()
	klog.V(4).Infof("Started syncing ControllerConfig %q (%v)", key, startTime)

	if key != ctrlcommon.ControllerConfigName {
		// In theory there are no other ControllerConfigs other than the machine-config one
		// but to future-proof just in case, we don't need to sync on other changes
		return nil
	}

	// Step 1: Get the latest ControllerConfig from the informer's cache.
	// The informer watches the API server and caches ControllerConfig updates locally.
	controllerConfig, err := dn.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return fmt.Errorf("could not get ControllerConfig: %v", err)
	}

	if dn.node == nil {
		// Node has not yet initialized, wait to resync
		dn.enqueueControllerConfigAfter(controllerConfig, ccRequeueDelay)
		return nil
	}

	// Step 2: Check if this is a new version of ControllerConfig or a cert rotation event.
	// We track the last resourceVersion we processed via a node annotation to avoid
	// redundant work.
	//
	// Write the latest cert to disk, if the controllerconfig resourceVersion has updated
	// Also annotate the latest config we've seen, so as to not write unnecessarily
	currentNodeControllerConfigResource := dn.node.Annotations[constants.ControllerConfigResourceVersionKey]
	var cmErr error
	var data []byte
	kubeConfigDiff := false
	onDiskKC := clientcmdv1.Config{}

	// Step 3: Decide if we need to process this update.
	// We process if:
	// a) The resourceVersion changed (new config data), OR
	// b) ServiceCARotateAnnotation is "true" (cert rotation in progress)
	klog.V(4).Infof("Certificate sync: checking if sync needed (current node rv=%s, new rv=%s, annotation=%s)",
		currentNodeControllerConfigResource,
		controllerConfig.ObjectMeta.ResourceVersion,
		controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation])

	if currentNodeControllerConfigResource != controllerConfig.ObjectMeta.ResourceVersion || controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] == ctrlcommon.ServiceCARotateTrue {
		klog.Infof("Certificate sync: processing ControllerConfig update (rv %s -> %s)",
			currentNodeControllerConfigResource, controllerConfig.ObjectMeta.ResourceVersion)
		pathToData := make(map[string][]byte)
		kubeAPIServerServingCABytes := controllerConfig.Spec.KubeAPIServerServingCAData
		cloudCA := controllerConfig.Spec.CloudProviderCAData
		pathToData[caBundleFilePath] = kubeAPIServerServingCABytes
		pathToData[cloudCABundleFilePath] = cloudCA
		var err error
		var cm *corev1.ConfigMap
		var fullCA []string

		// If the ControllerConfig version changed, we should sync our OS image
		// pull secrets, since they could have changed.
		if err := dn.syncInternalRegistryPullSecrets(controllerConfig); err != nil {
			return err
		}

		// Step 4: PROACTIVE CA SYNC - Always Keep Kubeconfig In Sync
		//
		// This is the critical fix for the race condition bug.
		// Instead of waiting to catch ServiceCARotateAnnotation="true" (which can be missed
		// if the informer breaks), we proactively update kubeconfig whenever the CA bundle
		// changes in ControllerConfig.
		//
		// Why this eliminates the race condition:
		// 1. We update kubeconfig on EVERY ControllerConfig resourceVersion change
		// 2. We don't rely on catching a specific annotation value
		// 3. We exit BEFORE the old cert expires (during overlap period)
		// 4. Even if informer breaks temporarily, we catch up on next successful sync
		//
		// This preserves the defer logic:
		// - Localhost signers: Update kubeconfig but don't exit (deferred)
		// - Critical signers (lb-signer): Update kubeconfig AND exit immediately
		//
		if currentNodeControllerConfigResource != controllerConfig.ObjectMeta.ResourceVersion {
			klog.V(4).Infof("Proactive CA sync: ControllerConfig resourceVersion changed, checking if CA bundle changed")

			// Check if the CA bundle actually changed
			newCABundle := kubeAPIServerServingCABytes

			// Read current kubeconfig from disk
			kcBytes, err := os.ReadFile(kubeConfigPath)
			if err != nil && !os.IsNotExist(err) {
				klog.Errorf("Proactive CA sync: failed to read kubeconfig for CA comparison: %v", err)
				return fmt.Errorf("failed to read kubeconfig for CA comparison: %w", err)
			}

			if kcBytes == nil {
				klog.V(4).Infof("Proactive CA sync: kubeconfig does not exist yet, skipping CA comparison")
			} else if newCABundle == nil {
				klog.V(4).Infof("Proactive CA sync: new CA bundle is nil, skipping CA comparison")
			}

			if kcBytes != nil && newCABundle != nil {
				var diskKC clientcmdv1.Config
				if err := yaml.Unmarshal(kcBytes, &diskKC); err != nil {
					return fmt.Errorf("failed to unmarshal kubeconfig for CA comparison: %w", err)
				}

				// Compare CA bundles
				oldCAHash := fmt.Sprintf("%x", sha256.Sum256(diskKC.Clusters[0].Cluster.CertificateAuthorityData))[:16]
				newCAHash := fmt.Sprintf("%x", sha256.Sum256(newCABundle))[:16]
				klog.V(4).Infof("Proactive CA sync: comparing CA bundles (old hash=%s, new hash=%s)", oldCAHash, newCAHash)

				if !bytes.Equal(bytes.TrimSpace(diskKC.Clusters[0].Cluster.CertificateAuthorityData), bytes.TrimSpace(newCABundle)) {
					klog.Infof("Proactive CA sync: CA bundle changed in ControllerConfig (old=%s, new=%s) - analyzing changes", oldCAHash, newCAHash)

					// Parse both CA bundles to determine which certs changed
					oldCerts := strings.SplitAfter(strings.TrimSpace(string(diskKC.Clusters[0].Cluster.CertificateAuthorityData)), "-----END CERTIFICATE-----")
					newCerts := strings.SplitAfter(strings.TrimSpace(string(newCABundle)), "-----END CERTIFICATE-----")

					// Find new/updated certificates
					var addedCerts []string
					for _, newCert := range newCerts {
						found := false
						for _, oldCert := range oldCerts {
							if oldCert == newCert {
								found = true
								break
							}
						}
						if !found && strings.TrimSpace(newCert) != "" {
							addedCerts = append(addedCerts, newCert)
						}
					}

					if len(addedCerts) > 0 {
						klog.Infof("Proactive CA sync: found %d new/updated certificates", len(addedCerts))

						// Step 4a: Always update kubeconfig files on disk (stay in sync)
						diskKC.Clusters[0].Cluster.CertificateAuthorityData = newCABundle
						newKCData, err := yaml.Marshal(diskKC)
						if err != nil {
							return fmt.Errorf("failed to marshal kubeconfig: %w", err)
						}
						pathToData[kubeConfigPath] = newKCData
						klog.Infof("Proactive CA sync: prepared /etc/kubernetes/kubeconfig update")

						// Step 4b: Analyze which certificates changed to determine if we should exit
						shouldExit := false
						deferReason := ""

						for _, certPEM := range addedCerts {
							block, _ := pem.Decode([]byte(certPEM))
							if block == nil {
								continue
							}
							cert, err := x509.ParseCertificate(block.Bytes)
							if err != nil {
								klog.Warningf("Proactive CA sync: failed to parse cert: %v", err)
								continue
							}

							klog.Infof("Proactive CA sync: analyzing new cert with CommonName=%s", cert.Subject.CommonName)

							// Only defer for localhost signers (preserve existing defer logic)
							if strings.Contains(cert.Subject.CommonName, "kube-apiserver-localhost-signer") ||
								strings.Contains(cert.Subject.CommonName, "openshift-kube-apiserver-operator_localhost-recovery-serving-signer") {
								deferReason = fmt.Sprintf("localhost signer: %s", cert.Subject.CommonName)
								klog.Infof("Proactive CA sync: %s is a localhost signer - will defer exit", cert.Subject.CommonName)
							} else {
								// Critical cert changed (e.g., lb-signer) - must exit immediately
								klog.Infof("Proactive CA sync: %s is a critical signer - will exit after updating kubeconfig", cert.Subject.CommonName)
								shouldExit = true
							}
						}

						// Step 4c: Write updated kubeconfig to disk
						if err := writeToDisk(pathToData); err != nil {
							return fmt.Errorf("proactive CA sync: failed to write kubeconfig: %w", err)
						}
						klog.Infof("Proactive CA sync: wrote updated /etc/kubernetes/kubeconfig to disk")

						// Step 4d: Update kubelet's kubeconfig
						kubeletKCBytes, err := os.ReadFile("/var/lib/kubelet/kubeconfig")
						if err != nil && !os.IsNotExist(err) {
							return fmt.Errorf("proactive CA sync: failed to read kubelet kubeconfig: %w", err)
						}
						if kubeletKCBytes != nil {
							var kubeletKC clientcmdv1.Config
							if err := yaml.Unmarshal(kubeletKCBytes, &kubeletKC); err != nil {
								return fmt.Errorf("proactive CA sync: failed to unmarshal kubelet kubeconfig: %w", err)
							}
							kubeletKC.Clusters[0].Cluster.CertificateAuthorityData = newCABundle
							kubeletKCData, err := yaml.Marshal(kubeletKC)
							if err != nil {
								return fmt.Errorf("proactive CA sync: failed to marshal kubelet kubeconfig: %w", err)
							}
							if err := writeToDisk(map[string][]byte{"/var/lib/kubelet/kubeconfig": kubeletKCData}); err != nil {
								return fmt.Errorf("proactive CA sync: failed to write kubelet kubeconfig: %w", err)
							}
							klog.Infof("Proactive CA sync: wrote updated /var/lib/kubelet/kubeconfig to disk")
						}

						// Step 4e: Exit if a critical cert changed
						if shouldExit {
							klog.Infof("Proactive CA sync: critical cert changed - restarting kubelet and exiting MCD")
							logSystem("Proactive CA sync: restarting kubelet due to CA bundle change")

							if err := runCmdSync("systemctl", "stop", "kubelet"); err != nil {
								return fmt.Errorf("proactive CA sync: failed to stop kubelet: %w", err)
							}
							if err := runCmdSync("systemctl", "daemon-reload"); err != nil {
								return fmt.Errorf("proactive CA sync: failed to daemon-reload: %w", err)
							}
							if err := runCmdSync("systemctl", "start", "kubelet"); err != nil {
								return fmt.Errorf("proactive CA sync: failed to start kubelet: %w", err)
							}

							klog.Infof("Proactive CA sync: kubelet restarted successfully")
							logSystem("Proactive CA sync: exiting MCD to reload certificates")
							klog.Infof("Proactive CA sync: exiting machine-config-daemon to reload certificates after CA change")
							os.Exit(0)
						} else {
							// Deferred case - kubeconfig updated but no exit
							klog.Infof("Proactive CA sync: kubeconfig updated, exit deferred (%s)", deferReason)
							logSystem("Proactive CA sync: kubeconfig updated, restart deferred for %s", deferReason)
						}
					}
				} else {
					klog.V(4).Infof("Proactive CA sync: CA bundle unchanged (hash=%s), no action needed", oldCAHash)
				}
			}
		} else {
			klog.V(4).Infof("Proactive CA sync: skipping (resourceVersion unchanged: %s)", currentNodeControllerConfigResource)
		}

		// Step 5: LEGACY CERTIFICATE ROTATION HANDLING BLOCK (Fallback)
		//
		// This block handles the ORIGINAL rotation mechanism via ServiceCARotateAnnotation.
		// It's now a FALLBACK - the proactive CA sync above should handle most cases.
		//
		// Why keep this block?
		// 1. Backward compatibility - other components might rely on this flow
		// 2. Handles rotation via kubeconfig-data ConfigMap (different source than ControllerConfig.Spec)
		// 3. Provides redundancy if proactive sync misses something
		//
		// Note: If proactive sync already handled the rotation and exited, we never reach this code.
		// If we do reach it, it means either:
		// - Proactive sync deferred (localhost signer), and now we're handling via annotation
		// - We caught the annotation before proactive sync detected the CA change
		//
		// Conditions to enter this block:
		// a) ServiceCARotateAnnotation == "true" (rotation event triggered)
		// b) Node annotation doesn't match (we haven't processed this rotation yet)
		//
		if controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] == ctrlcommon.ServiceCARotateTrue && dn.node.Annotations[constants.ControllerConfigSyncServerCA] != controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] {
			klog.Infof("Certificate rotation detected: ServiceCARotateAnnotation=%s, node annotation=%s",
				controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation],
				dn.node.Annotations[constants.ControllerConfigSyncServerCA])

			// Step 4a: Fetch the NEW CA bundle from the kubeconfig-data ConfigMap.
			// This ConfigMap is maintained by cluster-kube-apiserver-operator and contains
			// the authoritative, up-to-date CA bundle for the API server.
			cm, cmErr = dn.kubeClient.CoreV1().ConfigMaps("openshift-machine-config-operator").Get(context.TODO(), "kubeconfig-data", v1.GetOptions{})
			if cmErr != nil {
				klog.Errorf("Error retrieving kubeconfig-data. %v", cmErr)
			} else {
				// Step 4b: Extract the ca-bundle.crt from the ConfigMap
				data, err = cmToData(cm, "ca-bundle.crt")
				if err != nil {
					klog.Errorf("kubeconfig-data ConfigMap not populated yet. %v", err)
				} else if data != nil {
					// Step 4c: Read the current on-disk kubeconfig at /etc/kubernetes/kubeconfig
					// This is the MCD's own kubeconfig file.
					kcBytes, err := os.ReadFile(kubeConfigPath)
					if err != nil {
						return err
					}
					if kcBytes != nil {
						err = yaml.Unmarshal(kcBytes, &onDiskKC)
						if err != nil {
							return fmt.Errorf("could not unmarshal kubeconfig into struct. Data: %s, Error: %v", string(kcBytes), err)
						}

						// Step 4d: Compare the CA bundle in the ConfigMap vs on-disk kubeconfig.
						// If they differ, we need to update the kubeconfig files.
						kubeConfigDiff = !bytes.Equal(bytes.TrimSpace(onDiskKC.Clusters[0].Cluster.CertificateAuthorityData), bytes.TrimSpace(data))
						klog.Infof("Certificate rotation: kubeConfigDiff=%t (CA bundle differs between ConfigMap and on-disk kubeconfig)", kubeConfigDiff)

						// Step 4e: Parse both CA bundles into individual certificates.
						// A CA bundle is a PEM file containing multiple concatenated certificates.
						// We split them apart to determine which specific certs were added/updated.
						//
						// We should always write the latest certs from the configmap onto disk, but we should check what was changed/modified
						// if any certs were added or updated, determine if we need to defer the kubelet restarting
						certsConfigmap := strings.SplitAfter(strings.TrimSpace(string(data)), "-----END CERTIFICATE-----")
						certsDisk := strings.SplitAfter(strings.TrimSpace(string(onDiskKC.Clusters[0].Cluster.CertificateAuthorityData)), "-----END CERTIFICATE-----")
						var addedOrUpdatedCAs []string
						klog.Infof("Certificate rotation: found %d certs in ConfigMap, %d certs on disk", len(certsConfigmap), len(certsDisk))

						// Step 4f: Find which certificates are NEW (in ConfigMap but not on disk).
						// During rotation, the old cert is kept alongside the new one for a period,
						// so clients using either cert can still connect.
						for _, cert := range certsConfigmap {
							found := false
							for _, onDiskCert := range certsDisk {
								if onDiskCert == cert {
									found = true
									break
								}
							}
							if !found {
								addedOrUpdatedCAs = append(addedOrUpdatedCAs, cert)
								klog.Info("Certificate rotation: found new or updated cert (not present on disk)")
							}

							b, _ := pem.Decode([]byte(cert))
							if b == nil {
								klog.Infof("Unable to decode cert into a pem block. Cert is either empty or invalid.")
								break
							}
							_, err := x509.ParseCertificate(b.Bytes)
							if err != nil {
								logSystem("Malformed Cert, not syncing: %s", cert)
								continue
							}

							fullCA = append(fullCA, cert)
						}

						// Step 4g: Examine each new certificate to decide if we should defer kubelet restart.
						//
						// ## Why defer some rotations?
						// During OpenShift upgrades, localhost-signer certs rotate frequently and unpredictably.
						// These certs are only used for localhost recovery connections and don't affect normal
						// cluster operation. Restarting kubelet for every localhost-signer rotation would cause
						// unnecessary node disruptions during upgrades.
						//
						// ## Why NOT defer loadbalancer-serving-signer?
						// The loadbalancer-serving-signer cert signs the API server's public certificate
						// (api.cluster.com, api-int.cluster.com). When this cert rotates:
						// - The MCD's Kubernetes client has the OLD CA bundle in memory
						// - The API server starts using the NEW certificate
						// - The MCD cannot verify the API server's cert (x509: certificate signed by unknown authority)
						// - The MCD MUST exit so kubelet restarts it with the fresh CA bundle
						//
						klog.Infof("Certificate rotation: processing %d added/updated certs to determine if kubelet restart should be deferred", len(addedOrUpdatedCAs))
						dn.deferKubeletRestart = true // Optimistic: assume we can defer unless proven otherwise
						for _, cert := range addedOrUpdatedCAs {
							b, _ := pem.Decode([]byte(cert))
							if b == nil {
								klog.Infof("Unable to decode cert into a pem block. Cert is either empty or invalid.")
								break
							}
							c, err := x509.ParseCertificate(b.Bytes)
							if err != nil {
								logSystem("Malformed Cert, not syncing: %s", cert)
								continue
							}
							logSystem("Cert not found in kubeconfig. This means we need to write to disk. Subject is: %s", c.Subject.CommonName)
							klog.Infof("Certificate rotation: examining cert with CommonName=%s", c.Subject.CommonName)

							// Step 4h: THE FIX - Only defer for localhost signers.
							// Localhost signers rotate randomly during upgrades - defer restart to avoid unnecessary kubelet restarts.
							// All other signers (including lb-signer) require immediate restart to reload MCD's Kubernetes client.
							if !strings.Contains(c.Subject.CommonName, "kube-apiserver-localhost-signer") && !strings.Contains(c.Subject.CommonName, "openshift-kube-apiserver-operator_localhost-recovery-serving-signer") {
								logSystem("Need to restart kubelet")
								klog.Infof("Certificate rotation: cert %s requires immediate kubelet restart (not a localhost signer)", c.Subject.CommonName)
								dn.deferKubeletRestart = false
							} else {
								logSystem("Deferring kubelet restart for localhost signer")
								klog.Infof("Certificate rotation: cert %s is a localhost signer, deferring kubelet restart", c.Subject.CommonName)
							}
						}

						if kubeConfigDiff {
							// Step 4i: Update /etc/kubernetes/kubeconfig (MCD's kubeconfig) with the new CA bundle.
							// This writes the new CA bundle to disk but doesn't reload the MCD's in-memory client.
							// The MCD will still exit and restart to reload its client.
							klog.Infof("Certificate rotation: writing updated CA bundle to %s (deferKubeletRestart=%t)", kubeConfigPath, dn.deferKubeletRestart)
							var newData []byte
							if onDiskKC.Clusters == nil {
								return errors.New("clusters cannot be nil")
							}
							// use ALL data we have, including new certs (both old and new for overlap period)
							onDiskKC.Clusters[0].Cluster.CertificateAuthorityData = []byte(strings.Join(fullCA, ""))
							newData, err = yaml.Marshal(onDiskKC)
							if err != nil {
								return fmt.Errorf("could not marshal kubeconfig into bytes. Error: %v", err)
							}

							pathToData[kubeConfigPath] = newData
							klog.Infof("Certificate rotation: prepared kubeconfig update with %d certs for %s", len(fullCA), kubeConfigPath)
						}
					} else {
						klog.Info("Could not read kubeconfig file, or data does not need to be changed")
					}
				}
			}
		}
		if err := writeToDisk(pathToData); err != nil {
			return err
		}

		mergedData := append([]mcfgv1.ImageRegistryBundle{}, append(controllerConfig.Spec.ImageRegistryBundleData, controllerConfig.Spec.ImageRegistryBundleUserData...)...)

		entries, err := os.ReadDir("/etc/docker/certs.d")
		if err != nil {
			klog.Errorf("/etc/docker/certs.d does not exist yet: %v", err)
		} else {
			for _, entry := range entries {
				if entry.IsDir() {
					stillExists := false
					for _, CA := range mergedData {
						// if one of our spec CAs matches the existing file, we are good.
						if CA.File == entry.Name() {
							stillExists = true
						}
					}
					if !stillExists {
						if err := os.RemoveAll(filepath.Join("/etc/docker/certs.d", entry.Name())); err != nil {
							klog.Warningf("Could not remove old certificate: %s", filepath.Join("/etc/docker/certs.d", entry.Name()))
						}
					}
				}
			}

			for _, CA := range controllerConfig.Spec.ImageRegistryBundleData {
				caFile := strings.ReplaceAll(CA.File, "..", ":")
				if err := os.MkdirAll(filepath.Join(imageCAFilePath, caFile), defaultDirectoryPermissions); err != nil {
					return err
				}
				if err := writeFileAtomicallyWithDefaults(filepath.Join(imageCAFilePath, caFile, "ca.crt"), CA.Data); err != nil {
					return err
				}
			}

			for _, CA := range controllerConfig.Spec.ImageRegistryBundleUserData {
				caFile := strings.ReplaceAll(CA.File, "..", ":")
				if err := os.MkdirAll(filepath.Join(imageCAFilePath, caFile), defaultDirectoryPermissions); err != nil {
					return err
				}
				if err := writeFileAtomicallyWithDefaults(filepath.Join(imageCAFilePath, caFile, "ca.crt"), CA.Data); err != nil {
					return err
				}
			}

		}
	}

	// Step 5: Check if this is the MAIN ROTATION HANDLING block.
	// Read the node's last recorded value of ServiceCARotateAnnotation.
	oldAnno := dn.node.Annotations[constants.ControllerConfigSyncServerCA]

	klog.Infof("Certificate was synced from controllerconfig resourceVersion %s", controllerConfig.ObjectMeta.ResourceVersion)
	rotationInProgress := false

	// Step 6: MAIN ROTATION HANDLING BLOCK
	// This block handles the actual kubelet restart and MCD exit.
	//
	// ## Conditions to enter this block:
	// a) ServiceCARotateAnnotation == "true" (rotation event triggered by cluster-kube-apiserver-operator)
	// b) oldAnno != "true" (we haven't already processed this rotation on this node)
	// c) cmErr == nil (successfully fetched kubeconfig-data ConfigMap)
	// d) kubeConfigDiff == true (CA bundle actually changed)
	//
	// ## THIS IS WHERE THE BUG HAPPENS:
	// If the MCD's informer breaks (x509 errors) BEFORE it receives the ControllerConfig update
	// with ServiceCARotateAnnotation="true", this condition is FALSE and the block is skipped.
	// The MCD then gets stuck with stale certs and can't recover.
	//
	if controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] == ctrlcommon.ServiceCARotateTrue && oldAnno != controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] && cmErr == nil && kubeConfigDiff {
		klog.Infof("Certificate rotation: entering main rotation block (annotation=%s, oldAnno=%s, cmErr=%v, kubeConfigDiff=%t)",
			controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation], oldAnno, cmErr, kubeConfigDiff)
		if len(onDiskKC.Clusters[0].Cluster.CertificateAuthorityData) > 0 {
			// Step 6a: Update kubelet's kubeconfig at /var/lib/kubelet/kubeconfig
			// This is separate from the MCD's kubeconfig (/etc/kubernetes/kubeconfig).
			klog.Infof("Certificate rotation: updating kubelet kubeconfig with new CA bundle")
			// Always update kubelet's kubeconfig with new CA bundle
			f, err := os.ReadFile("/var/lib/kubelet/kubeconfig")
			if err != nil && os.IsNotExist(err) {
				klog.Warningf("Failed to get kubeconfig file: %v", err)
				return err
			} else if err != nil {
				return fmt.Errorf("unexpected error reading kubeconfig file, %v", err)
			}
			kubeletKC := clientcmdv1.Config{}
			err = yaml.Unmarshal(f, &kubeletKC)
			if err != nil {
				return err
			}
			// set CA data to the one we just parsed above, the rest of the data should be preserved.
			kubeletKC.Clusters[0].Cluster.CertificateAuthorityData = onDiskKC.Clusters[0].Cluster.CertificateAuthorityData
			newData, err := yaml.Marshal(kubeletKC)
			if err != nil {
				return fmt.Errorf("could not marshal kubeconfig into bytes. Error: %v", err)
			}
			filesToWrite := make(map[string][]byte)
			filesToWrite["/var/lib/kubelet/kubeconfig"] = newData
			err = writeToDisk(filesToWrite)
			if err != nil {
				return err
			}
			klog.Infof("Certificate rotation: wrote kubelet kubeconfig to disk")

			// Step 6b: Restart kubelet and exit MCD (UNLESS deferred).
			// Restart kubelet only if deferKubeletRestart is false
			if !dn.deferKubeletRestart {
				// Step 6c: Restart kubelet to reload its client with the new CA bundle.
				klog.Infof("Certificate rotation: deferKubeletRestart=false, proceeding with kubelet restart and MCD exit")
				logSystem("restarting kubelet due to server-ca rotation")
				klog.Infof("Certificate rotation: stopping kubelet")
				if err := runCmdSync("systemctl", "stop", "kubelet"); err != nil {
					return err
				}

				klog.Infof("Certificate rotation: running daemon-reload")
				if err := runCmdSync("systemctl", "daemon-reload"); err != nil {
					return err
				}

				klog.Infof("Certificate rotation: starting kubelet")
				if err := runCmdSync("systemctl", "start", "kubelet"); err != nil {
					return err
				}
				klog.Infof("Certificate rotation: kubelet restarted successfully")

				// Step 6d: Update node annotation to record that we processed this rotation.
				// Update node annotation only after successfully restarting kubelet.
				// This ensures we retry if MCD restarts before completing the rotation.
				annos := map[string]string{
					constants.ControllerConfigResourceVersionKey: controllerConfig.ObjectMeta.ResourceVersion,
				}
				if dn.node.Annotations[constants.ControllerConfigSyncServerCA] != controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] {
					annos[constants.ControllerConfigSyncServerCA] = controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation]
				}
				klog.Infof("Certificate rotation: updating node annotations (ControllerConfigResourceVersion=%s, ControllerConfigSyncServerCA=%s)",
					controllerConfig.ObjectMeta.ResourceVersion, controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation])
				if _, err := dn.nodeWriter.SetAnnotations(annos); err != nil {
					return fmt.Errorf("failed to set annotations on node: %w", err)
				}
				klog.Infof("Certificate rotation: node annotations updated successfully")

				// Step 6e: THE CRITICAL FIX - Exit the MCD pod.
				//
				// ## Why must the MCD exit?
				// The MCD's Kubernetes client (dn.kubeClient, dn.nodeWriter, informers) was initialized
				// at pod startup by loading /etc/kubernetes/kubeconfig. These clients cache the CA bundle
				// in memory and CANNOT be reloaded without restarting the process.
				//
				// After the cert rotates:
				// - We updated /etc/kubernetes/kubeconfig with the new CA bundle on disk
				// - But the MCD's in-memory clients still have the OLD CA bundle
				// - The API server is now using the NEW certificate
				// - The MCD cannot verify the API server's cert -> x509 errors
				//
				// By exiting, we force kubelet (which manages the MCD DaemonSet) to restart the pod.
				// The new pod will load /etc/kubernetes/kubeconfig fresh and get the new CA bundle.
				//
				// Exit MCD after restarting kubelet to reload MCD's own certificates.
				// The NodeWriter's Kubernetes client was initialized at startup with the old CA bundle,
				// and it cannot be reloaded without restarting the pod. Since MCD runs as a DaemonSet,
				// kubelet will automatically restart it.
				logSystem("Exiting machine-config-daemon to reload certificates after rotation")
				klog.Infof("Exiting machine-config-daemon to reload certificates after server CA rotation")
				os.Exit(0)
			}
			// Step 6f: DEFERRED RESTART CASE (localhost signers only).
			// Deferred restart case: kubeconfig is updated but kubelet restart is deferred.
			// DO NOT exit MCD or update annotation yet. Keep rotation in progress to allow retry.
			// The deferred restart will be triggered by:
			// - Next sync when conditions change (e.g., all certs present, or new rotation)
			// - stopCh handler (pkg/daemon/daemon.go) when MCD receives shutdown signal
			// - x509 error handler when kubelet encounters cert errors
			// This preserves the in-memory deferKubeletRestart state so the restart can happen later.
			rotationInProgress = true
			klog.Infof("Certificate rotation: deferKubeletRestart=true, entering deferred rotation state (not exiting MCD, not updating annotation)")
			logSystem("Deferring kubelet restart - kubeconfig updated but kubelet will pick up " +
				"changes on next restart or when triggered by x509 errors")
			klog.Infof("Certificate rotation: kubelet kubeconfig updated with new CA bundle, " +
				"restart deferred for localhost signers")
		}
	}

	// Step 7: Update node annotation for normal (non-rotation) syncs.
	// Only update annotation if not in deferred rotation state
	if !rotationInProgress {
		klog.Infof("Certificate rotation: not in deferred state, updating node annotations")
		annos := map[string]string{
			constants.ControllerConfigResourceVersionKey: controllerConfig.ObjectMeta.ResourceVersion,
		}
		// Also update ServiceCA annotation if it changed, to mark deferred rotations complete
		if dn.node.Annotations[constants.ControllerConfigSyncServerCA] !=
			controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation] {
			annos[constants.ControllerConfigSyncServerCA] =
				controllerConfig.Annotations[ctrlcommon.ServiceCARotateAnnotation]
		}
		if _, err := dn.nodeWriter.SetAnnotations(annos); err != nil {
			return fmt.Errorf("failed to set annotations on node: %w", err)
		}
		klog.Infof("Certificate rotation: final annotation update complete")
	} else {
		klog.Infof("Certificate rotation: skipping annotation update (rotationInProgress=true)")
	}

	klog.V(4).Infof("Finished syncing ControllerConfig %q (%v)", key, time.Since(startTime))
	return nil
}

// Syncs the OS image pull secrets to disk under
// /etc/mco/internal-registry-pull-secret.json using the contents of the
// ControllerConfig. This will run during the certificate_writer sync loop
// as well as during an OS update. Because this can execute across multiple
// Goroutines, a Daemon-level mutex (osImageMux) is used to ensure that only
// one call can execute at any given time.
func (dn *Daemon) syncInternalRegistryPullSecrets(controllerConfig *mcfgv1.ControllerConfig) error {
	dn.osImageMux.Lock()
	defer dn.osImageMux.Unlock()

	if controllerConfig == nil {
		cfg, err := dn.ccLister.Get(ctrlcommon.ControllerConfigName)
		if err != nil {
			return fmt.Errorf("could not get ControllerConfig: %v", err)
		}

		controllerConfig = cfg
	}

	if err := writeToDisk(map[string][]byte{internalRegistryAuthFile: controllerConfig.Spec.InternalRegistryPullSecret}); err != nil {
		return fmt.Errorf("could not write image pull secret data to node filesystem: %w", err)
	}

	klog.V(4).Infof("Synced image registry secrets to node filesystem in %s", internalRegistryAuthFile)

	return nil
}

func cmToData(cm *corev1.ConfigMap, key string) ([]byte, error) {
	if bd, bdok := cm.BinaryData[key]; bdok {
		return bd, nil
	}
	if d, dok := cm.Data[key]; dok {
		raw, err := base64.StdEncoding.DecodeString(d)
		if err != nil {
			return []byte(d), nil
		}
		return raw, nil
	}
	return nil, fmt.Errorf("%s not found in %s/%s", key, cm.Namespace, cm.Name)
}

func writeToDisk(pathToData map[string][]byte) error {
	for bundle, data := range pathToData {
		if !strings.HasSuffix(string(data), "\n") {
			bString := string(data) + "\n"
			data = []byte(bString)
		}
		if Finfo, err := os.Stat(bundle); err == nil {
			var mode os.FileMode
			Dinfo, err := os.Stat(filepath.Dir(bundle))
			if err != nil {
				mode = defaultDirectoryPermissions
			} else {
				mode = Dinfo.Mode()
			}
			// we need to make sure we honor the mode of that file
			if err := writeFileAtomically(bundle, data, mode, Finfo.Mode(), -1, -1); err != nil {
				return err
			}
		} else {
			if err := writeFileAtomicallyWithDefaults(bundle, data); err != nil {
				return err
			}
		}
	}
	return nil
}
