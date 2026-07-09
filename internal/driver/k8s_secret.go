package driver

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// PVC annotations read by CreateVolume.
	annotationSecretName = "drive9.ai/secret-name"
	annotationRemoteRoot = "drive9.ai/remote-root"

	// Parameters injected by the external-provisioner when
	// --extra-create-metadata is enabled.
	paramPVCName      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc/namespace"

	// PV volumeAttributes keys for Secret reference (non-sensitive).
	attrSecretName      = "secretName"
	attrSecretNamespace = "secretNamespace"
)

// pvcSecretRef holds the resolved Secret reference from a PVC annotation.
type pvcSecretRef struct {
	SecretName      string
	SecretNamespace string
	RemoteRoot      string // from drive9.ai/remote-root annotation; defaults to "/"
}

// resolveSecretRefFromPVC reads the PVC annotation drive9.ai/secret-name and
// returns the Secret reference. Returns an InvalidArgument error if the
// required annotation is missing.
func resolveSecretRefFromPVC(ctx context.Context, k8s kubernetes.Interface, pvcName, pvcNamespace string) (pvcSecretRef, error) {
	pvc, err := k8s.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return pvcSecretRef{}, status.Errorf(codes.Internal, "read PVC %s/%s: %v", pvcNamespace, pvcName, err)
	}

	secretName := strings.TrimSpace(pvc.Annotations[annotationSecretName])
	if secretName == "" {
		return pvcSecretRef{}, status.Errorf(codes.InvalidArgument,
			"PVC %s/%s is missing required annotation %q: add the annotation to specify which Secret contains Drive9 credentials",
			pvcNamespace, pvcName, annotationSecretName)
	}

	remoteRoot := strings.TrimSpace(pvc.Annotations[annotationRemoteRoot])
	if remoteRoot == "" {
		remoteRoot = defaultRemoteRoot
	}

	return pvcSecretRef{
		SecretName:      secretName,
		SecretNamespace: pvcNamespace,
		RemoteRoot:      remoteRoot,
	}, nil
}

// readCredentialsFromSecret fetches a Kubernetes Secret and extracts
// Drive9 credentials (server, apiKey). Returns an error if the Secret
// does not exist or is missing required keys.
func readCredentialsFromSecret(ctx context.Context, k8s kubernetes.Interface, secretName, secretNamespace string) (drive9Credentials, error) {
	secret, err := k8s.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return drive9Credentials{}, status.Errorf(codes.InvalidArgument,
			"read Secret %s/%s: %v", secretNamespace, secretName, err)
	}
	return credentialsFromK8sSecret(secret)
}

// credentialsFromK8sSecret extracts Drive9 credentials from a Kubernetes
// Secret object. Supports the same key variants as credentialsFromSecrets.
func credentialsFromK8sSecret(secret *corev1.Secret) (drive9Credentials, error) {
	secrets := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		secrets[k] = string(v)
	}
	creds, err := credentialsFromSecrets(secrets)
	if err != nil {
		return drive9Credentials{}, status.Errorf(codes.InvalidArgument,
			"Secret %s/%s: %v", secret.Namespace, secret.Name, err)
	}
	return creds, nil
}

// extractPVCRef reads the PVC name and namespace from CreateVolume
// parameters (injected by --extra-create-metadata). Returns an error
// if either is missing.
func extractPVCRef(params map[string]string) (name, namespace string, err error) {
	name = strings.TrimSpace(params[paramPVCName])
	namespace = strings.TrimSpace(params[paramPVCNamespace])
	if name == "" || namespace == "" {
		return "", "", status.Errorf(codes.InvalidArgument,
			"missing PVC metadata in CreateVolume parameters (%s, %s); "+
				"ensure the CSI provisioner is started with --extra-create-metadata",
			paramPVCName, paramPVCNamespace)
	}
	return name, namespace, nil
}

// resolveSecretRefFromPV looks up a PersistentVolume by its CSI volumeHandle
// and returns the Secret reference stored in its volumeAttributes. This is
// used by DeleteVolume, which only receives volumeID — no volumeContext.
func resolveSecretRefFromPV(ctx context.Context, k8s kubernetes.Interface, volumeID string) (secretName, secretNamespace string, err error) {
	attrs, err := resolveVolumeContextFromPV(ctx, k8s, "", volumeID)
	if err != nil {
		return "", "", err
	}
	name := strings.TrimSpace(attrs[attrSecretName])
	ns := strings.TrimSpace(attrs[attrSecretNamespace])
	if name != "" && ns != "" {
		return name, ns, nil
	}
	return "", "", status.Errorf(codes.FailedPrecondition,
		"no PV with CSI volumeHandle %q found with Secret reference in volumeAttributes", volumeID)
}

// resolveVolumeContextFromPV looks up a PersistentVolume by its CSI
// volumeHandle and returns a copy of its volumeAttributes. If driverName is
// non-empty, the PV must also match that CSI driver.
func resolveVolumeContextFromPV(ctx context.Context, k8s kubernetes.Interface, driverName string, volumeID string) (map[string]string, error) {
	pvList, err := k8s.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list PVs to resolve volume attributes for volume %s: %v", volumeID, err)
	}
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		csiSource := pv.Spec.CSI
		if csiSource == nil || csiSource.VolumeHandle != volumeID {
			continue
		}
		if driverName != "" && csiSource.Driver != driverName {
			return nil, status.Errorf(codes.FailedPrecondition,
				"PV %s for volume %s is owned by CSI driver %q, want %q",
				pv.Name, volumeID, csiSource.Driver, driverName)
		}
		attrs := make(map[string]string, len(csiSource.VolumeAttributes))
		for k, v := range csiSource.VolumeAttributes {
			attrs[k] = v
		}
		return attrs, nil
	}
	return nil, status.Errorf(codes.FailedPrecondition, "no PV with CSI volumeHandle %q found", volumeID)
}

// validateNoAPIKeyInAttributes checks that volumeAttributes do not
// contain any credential keys. This is a safety check — credentials
// must never be persisted in PV objects.
func validateNoAPIKeyInAttributes(attrs map[string]string) error {
	for _, key := range []string{"apiKey", "api_key", "DRIVE9_API_KEY", "server", "DRIVE9_SERVER"} {
		if v := strings.TrimSpace(attrs[key]); v != "" {
			return fmt.Errorf("volumeAttributes must not contain credential key %q", key)
		}
	}
	return nil
}
