package driver

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
