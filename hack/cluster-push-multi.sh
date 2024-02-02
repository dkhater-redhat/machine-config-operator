#!/usr/bin/env bash

# Build the MCO image for multiple architectures (arm64 and amd64)
# and push it to the cluster registry,
# then directly patch the deployments/daemonsets to use that image.
#
# Assumptions: You have set KUBECONFIG to point to your cluster.
# You are logged into the Red Hat VPN

set -xeuo pipefail

# Configuration variables
USERNAME="dkhater" # Change this to your DockerHub username or local registry prefix
BUILDER_NAME="mybuilder"
IMAGE_NAME_BASE="machine-config-operator"
IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
PROJECT_NAME="openshift-machine-config-operator"
TARGET_PLATFORMS="linux/amd64,linux/arm64"
LOCAL_IMAGE_NAME="${USERNAME}/${IMAGE_NAME_BASE}:multiarch-latest"
REMOTE_IMAGE_NAME="${PROJECT_NAME}/${IMAGE_NAME_BASE}:multiarch-${IMAGE_TAG}"

# check if route exists
if ! oc get route image-registry -n openshift-image-registry &>/dev/null; then
    oc create route reencrypt --service=image-registry -n openshift-image-registry
fi

# registry host configuration
REGISTRY_HOST=$(oc get -n openshift-image-registry -o json route/image-registry | jq -r ".spec.host")
INSECURE_REGISTRY_CONFIG="{\"insecure-registries\" : [\"$REGISTRY_HOST\"]}"

# Check if the internal registry is accessible
curl -k --head https://"${REGISTRY_HOST}" >/dev/null

# Path to the prep flag file
PREP_FLAG_FILE="/tmp/cluster-push-prep-done-$(date +%Y%m%d)"

# check if cluster-push-prep.sh has been run today
# registry to Docker Desktop's list of insecure registries
if [ ! -f "${PREP_FLAG_FILE}" ]; then
    echo "Running cluster-push-prep.sh for the first time today"
    ./hack/cluster-push-prep.sh
    ./hack/docker-setup
    # Mark that cluster-push-prep.sh has been run
    touch "${PREP_FLAG_FILE}"
else
    echo "cluster-push-prep.sh has already been run today"
fi

# Check if the custom builder exists
BUILDER_EXISTS=$(docker buildx ls | grep "$BUILDER_NAME")

# If the builder does not exist, create it
if [ -z "$BUILDER_EXISTS" ]; then
    echo "Builder $BUILDER_NAME does not exist, creating it..."
    docker buildx create --name "$BUILDER_NAME" --use
else
    echo "Builder $BUILDER_NAME exists, switching to use it..."
    # Ensure the builder is set to use
    docker buildx use "$BUILDER_NAME"
fi

# Build and push the image to the local Docker/Podman and then to the internal registry
# docker buildx build --platform "${TARGET_PLATFORMS}" -t "${LOCAL_IMAGE_NAME}" . --push

CACHE_TAG="cache-${IMAGE_NAME_BASE}:latest"
CACHE_REF="${REGISTRY_HOST}/${PROJECT_NAME}/${CACHE_TAG}"

# Build command with caching
docker buildx build \
  --platform "${TARGET_PLATFORMS}" \
  --tag "${LOCAL_IMAGE_NAME}" \
  --cache-from type=registry,ref="${CACHE_REF}" \
  --cache-to type=inline,mode=max,ref="${CACHE_REF}" \
  --push .
  
builder_secret_id=$(oc get -n "${PROJECT_NAME}" secret | grep '^builder-token-' | head -1 | cut -f 1 -d ' ')
secret=$(oc get -n "${PROJECT_NAME}" -o json secret/${builder_secret_id} | jq -r '.data.token' | base64 -d)

podman pull "${LOCAL_IMAGE_NAME}"
podman tag "${LOCAL_IMAGE_NAME}" "${REGISTRY_HOST}/${REMOTE_IMAGE_NAME}"
podman push --tls-verify=false --creds "unused:${secret}" "${LOCAL_IMAGE_NAME}" "${REGISTRY_HOST}/${REMOTE_IMAGE_NAME}"

digest=$(skopeo inspect --creds "unused:${secret}" --tls-verify=false "docker://${REGISTRY_HOST}/${REMOTE_IMAGE_NAME}" | jq -r '.Digest')
image_id="${REMOTE_IMAGE_NAME}@${digest}"

oc project "${PROJECT_NAME}"

IN_CLUSTER_NAME="image-registry.openshift-image-registry.svc:5000/${image_id}"

# Scale down the operator to avoid race conditions during the update
oc scale --replicas=0 deploy/machine-config-operator

# Patch the images.json
tmpf=$(mktemp)
oc get -o json configmap/machine-config-operator-images > "${tmpf}"
outf=$(mktemp)
python3 > "${outf}" <<EOF
import sys,json
cm=json.load(open("${tmpf}"))
images = json.loads(cm['data']['images.json'])
for k in images:
  if k.startswith('machineConfig'):
    images[k] = "${IN_CLUSTER_NAME}"
cm['data']['images.json'] = json.dumps(images)
json.dump(cm, sys.stdout)
EOF
oc replace -f "${outf}"
rm "${tmpf}" "${outf}"

# Patch deployments and daemonsets to use the new image
for x in operator controller server daemon; do
    patch=$(mktemp)
    cat >"${patch}" <<EOF
spec:
  template:
     spec:
       containers:
         - name: machine-config-${x}
           image: ${IN_CLUSTER_NAME}
EOF

    case $x in
        controller|operator)
            target=deploy/machine-config-${x}
            ;;
        daemon|server)
            target=daemonset/machine-config-${x}
            ;;
        *) echo "Unhandled $x" && exit 1
    esac

    oc patch "${target}" -p "$(cat ${patch})"
    rm "${patch}"
    echo "Patched ${target}"
done

# Scale up the operator
oc scale --replicas=1 deploy/machine-config-operator
