package driver

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveSecretRefFromPVC_HappyPath(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-a",
			Namespace: "default",
			Annotations: map[string]string{
				"drive9.ai/secret-name": "my-secret",
				"drive9.ai/remote-root": "/project-a",
			},
		},
	})

	ref, err := resolveSecretRefFromPVC(context.Background(), k8s, "pvc-a", "default")
	if err != nil {
		t.Fatalf("resolveSecretRefFromPVC error = %v", err)
	}
	if ref.SecretName != "my-secret" {
		t.Fatalf("SecretName = %q, want my-secret", ref.SecretName)
	}
	if ref.SecretNamespace != "default" {
		t.Fatalf("SecretNamespace = %q, want default", ref.SecretNamespace)
	}
	if ref.RemoteRoot != "/project-a" {
		t.Fatalf("RemoteRoot = %q, want /project-a", ref.RemoteRoot)
	}
}

func TestResolveSecretRefFromPVC_DefaultRemoteRoot(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-b",
			Namespace: "ns1",
			Annotations: map[string]string{
				"drive9.ai/secret-name": "drive9-secret",
			},
		},
	})

	ref, err := resolveSecretRefFromPVC(context.Background(), k8s, "pvc-b", "ns1")
	if err != nil {
		t.Fatalf("resolveSecretRefFromPVC error = %v", err)
	}
	if ref.RemoteRoot != "/" {
		t.Fatalf("RemoteRoot = %q, want /", ref.RemoteRoot)
	}
}

func TestResolveSecretRefFromPVC_MissingAnnotation(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pvc-no-annotation",
			Namespace: "default",
		},
	})

	_, err := resolveSecretRefFromPVC(context.Background(), k8s, "pvc-no-annotation", "default")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	// Error message should mention the annotation name.
	if msg := status.Convert(err).Message(); msg == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestResolveSecretRefFromPVC_PVCNotFound(t *testing.T) {
	k8s := fake.NewSimpleClientset()

	_, err := resolveSecretRefFromPVC(context.Background(), k8s, "nonexistent", "default")
	if err == nil {
		t.Fatal("expected error for missing PVC")
	}
}

func TestReadCredentialsFromSecret_HappyPath(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"server": []byte("https://api.drive9.ai"),
			"apiKey": []byte("test-key-123"),
		},
	})

	creds, err := readCredentialsFromSecret(context.Background(), k8s, "my-secret", "default")
	if err != nil {
		t.Fatalf("readCredentialsFromSecret error = %v", err)
	}
	if creds.Server != "https://api.drive9.ai" {
		t.Fatalf("Server = %q", creds.Server)
	}
	if creds.APIKey != "test-key-123" {
		t.Fatalf("APIKey = %q", creds.APIKey)
	}
}

func TestReadCredentialsFromSecret_SecretNotFound(t *testing.T) {
	k8s := fake.NewSimpleClientset()

	_, err := readCredentialsFromSecret(context.Background(), k8s, "nonexistent", "default")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestReadCredentialsFromSecret_MissingAPIKey(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"server": []byte("https://api.drive9.ai"),
		},
	})

	_, err := readCredentialsFromSecret(context.Background(), k8s, "bad-secret", "default")
	if err == nil {
		t.Fatal("expected error for missing apiKey")
	}
}

func TestReadCredentialsFromSecret_MissingServer(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-server",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"apiKey": []byte("test-key"),
		},
	})

	_, err := readCredentialsFromSecret(context.Background(), k8s, "no-server", "default")
	if err == nil {
		t.Fatal("expected error for missing server")
	}
}

func TestExtractPVCRef_HappyPath(t *testing.T) {
	name, ns, err := extractPVCRef(map[string]string{
		paramPVCName:      "my-pvc",
		paramPVCNamespace: "my-ns",
	})
	if err != nil {
		t.Fatalf("extractPVCRef error = %v", err)
	}
	if name != "my-pvc" || ns != "my-ns" {
		t.Fatalf("got name=%q ns=%q", name, ns)
	}
}

func TestExtractPVCRef_MissingParams(t *testing.T) {
	_, _, err := extractPVCRef(map[string]string{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestValidateNoAPIKeyInAttributes(t *testing.T) {
	if err := validateNoAPIKeyInAttributes(map[string]string{
		"remoteRoot": "/",
		"secretName": "my-secret",
	}); err != nil {
		t.Fatalf("unexpected error for clean attributes: %v", err)
	}

	for _, key := range []string{"apiKey", "api_key", "DRIVE9_API_KEY", "server", "DRIVE9_SERVER"} {
		if err := validateNoAPIKeyInAttributes(map[string]string{key: "leak"}); err == nil {
			t.Fatalf("expected error for credential key %q in attributes", key)
		}
	}
}

func TestResolveRecoveryPV(t *testing.T) {
	const (
		driverName = "csi.drive9.ai"
		volumeID   = "drive9-volume"
	)
	newPV := func(name string, driver string, handle string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{
				AccessModes: modes,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:           driver,
						VolumeHandle:     handle,
						VolumeAttributes: map[string]string{"remoteRoot": "/k8s/pvc/volume"},
					},
				},
			},
		}
	}
	tests := []struct {
		name     string
		objects  []kruntime.Object
		listErr  bool
		wantMNMW bool
		wantErr  bool
	}{
		{name: "RWX", objects: []kruntime.Object{newPV("rwx", driverName, volumeID, corev1.ReadWriteMany)}, wantMNMW: true},
		{name: "RWO", objects: []kruntime.Object{newPV("rwo", driverName, volumeID, corev1.ReadWriteOnce)}},
		{name: "RWOP", objects: []kruntime.Object{newPV("rwop", driverName, volumeID, corev1.ReadWriteOncePod)}},
		{name: "RWX with read only", objects: []kruntime.Object{newPV("rwx-mixed", driverName, volumeID, corev1.ReadOnlyMany, corev1.ReadWriteMany)}, wantMNMW: true},
		{name: "RWO and RWOP", objects: []kruntime.Object{newPV("rwo-mixed", driverName, volumeID, corev1.ReadWriteOnce, corev1.ReadWriteOncePod)}},
		{name: "missing", objects: []kruntime.Object{newPV("other", driverName, "another-volume", corev1.ReadWriteMany)}, wantErr: true},
		{name: "duplicate", objects: []kruntime.Object{
			newPV("first", driverName, volumeID, corev1.ReadWriteMany),
			newPV("second", driverName, volumeID, corev1.ReadWriteMany),
		}, wantErr: true},
		{name: "other driver before owner", objects: []kruntime.Object{
			newPV("other", "other.example.com", volumeID, corev1.ReadWriteMany),
			newPV("owner", driverName, volumeID, corev1.ReadWriteMany),
		}, wantMNMW: true},
		{name: "owner before other driver", objects: []kruntime.Object{
			newPV("owner", driverName, volumeID, corev1.ReadWriteOnce),
			newPV("other", "other.example.com", volumeID, corev1.ReadWriteMany),
		}},
		{name: "wrong driver", objects: []kruntime.Object{newPV("wrong", "other.example.com", volumeID, corev1.ReadWriteMany)}, wantErr: true},
		{name: "empty access modes", objects: []kruntime.Object{newPV("empty", driverName, volumeID)}, wantErr: true},
		{name: "read only", objects: []kruntime.Object{newPV("readonly", driverName, volumeID, corev1.ReadOnlyMany)}, wantErr: true},
		{name: "read only mixed with RWO", objects: []kruntime.Object{newPV("readonly-rwo", driverName, volumeID, corev1.ReadOnlyMany, corev1.ReadWriteOnce)}, wantErr: true},
		{name: "unsupported", objects: []kruntime.Object{newPV("unsupported", driverName, volumeID, corev1.PersistentVolumeAccessMode("UnknownWriter"))}, wantErr: true},
		{name: "list error", listErr: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			k8s := fake.NewSimpleClientset(test.objects...)
			if test.listErr {
				k8s.PrependReactor("list", "persistentvolumes", func(k8stesting.Action) (bool, kruntime.Object, error) {
					return true, nil, errors.New("injected PV list failure")
				})
			}

			attrs, mnmw, err := resolveRecoveryVolumeContextFromPV(
				context.Background(), k8s, driverName, volumeID,
			)
			if test.wantErr && err == nil {
				t.Fatal("resolveRecoveryVolumeContextFromPV() accepted unprovable recovery input")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("resolveRecoveryVolumeContextFromPV() error = %v", err)
			}
			if err == nil {
				if mnmw != test.wantMNMW {
					t.Fatalf("MNMW = %t, want %t", mnmw, test.wantMNMW)
				}
				if attrs["remoteRoot"] != "/k8s/pvc/volume" {
					t.Fatalf("remoteRoot = %q", attrs["remoteRoot"])
				}
				attrs["remoteRoot"] = "/mutated"
				for _, object := range test.objects {
					pv := object.(*corev1.PersistentVolume)
					if pv.Spec.CSI.Driver == driverName &&
						pv.Spec.CSI.VolumeHandle == volumeID &&
						pv.Spec.CSI.VolumeAttributes["remoteRoot"] != "/k8s/pvc/volume" {
						t.Fatal("returned attributes alias the PV map")
					}
				}
			} else if attrs != nil || mnmw {
				t.Fatalf("error returned attributes=%v MNMW=%t", attrs, mnmw)
			}

			pvLists := 0
			secretGets := 0
			for _, action := range k8s.Actions() {
				switch {
				case action.GetVerb() == "list" && action.GetResource().Resource == "persistentvolumes":
					pvLists++
				case action.GetVerb() == "get" && action.GetResource().Resource == "secrets":
					secretGets++
				}
			}
			if pvLists != 1 || secretGets != 0 {
				t.Fatalf("Kubernetes actions: PV lists=%d Secret gets=%d, want 1/0", pvLists, secretGets)
			}
		})
	}
}
