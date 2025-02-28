package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/operator/events/eventstesting"
	operatorhelpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	fakeapiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	opereatorfake "open-cluster-management.io/api/client/operator/clientset/versioned/fake"
	operatorapiv1 "open-cluster-management.io/api/operator/v1"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	fakeapiregistration "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"
)

func TestUpdateStatusCondition(t *testing.T) {
	nowish := metav1.Now()
	beforeish := metav1.Time{Time: nowish.Add(-10 * time.Second)}
	afterish := metav1.Time{Time: nowish.Add(10 * time.Second)}

	cases := []struct {
		name               string
		startingConditions []metav1.Condition
		newCondition       metav1.Condition
		expectedUpdated    bool
		expectedConditions []metav1.Condition
	}{
		{
			name:               "add to empty",
			startingConditions: []metav1.Condition{},
			newCondition:       newCondition("test", "True", "my-reason", "my-message", nil),
			expectedUpdated:    true,
			expectedConditions: []metav1.Condition{newCondition("test", "True", "my-reason", "my-message", nil)},
		},
		{
			name: "add to non-conflicting",
			startingConditions: []metav1.Condition{
				newCondition("two", "True", "my-reason", "my-message", nil),
			},
			newCondition:    newCondition("one", "True", "my-reason", "my-message", nil),
			expectedUpdated: true,
			expectedConditions: []metav1.Condition{
				newCondition("two", "True", "my-reason", "my-message", nil),
				newCondition("one", "True", "my-reason", "my-message", nil),
			},
		},
		{
			name: "change existing status",
			startingConditions: []metav1.Condition{
				newCondition("two", "True", "my-reason", "my-message", nil),
				newCondition("one", "True", "my-reason", "my-message", nil),
			},
			newCondition:    newCondition("one", "False", "my-different-reason", "my-othermessage", nil),
			expectedUpdated: true,
			expectedConditions: []metav1.Condition{
				newCondition("two", "True", "my-reason", "my-message", nil),
				newCondition("one", "False", "my-different-reason", "my-othermessage", nil),
			},
		},
		{
			name: "leave existing transition time",
			startingConditions: []metav1.Condition{
				newCondition("two", "True", "my-reason", "my-message", nil),
				newCondition("one", "True", "my-reason", "my-message", &beforeish),
			},
			newCondition:    newCondition("one", "True", "my-reason", "my-message", &afterish),
			expectedUpdated: false,
			expectedConditions: []metav1.Condition{
				newCondition("two", "True", "my-reason", "my-message", nil),
				newCondition("one", "True", "my-reason", "my-message", &beforeish),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeOperatorClient := opereatorfake.NewSimpleClientset(
				&operatorapiv1.ClusterManager{
					ObjectMeta: metav1.ObjectMeta{Name: "testmanagedcluster"},
					Status: operatorapiv1.ClusterManagerStatus{
						Conditions: c.startingConditions,
					},
				},
				&operatorapiv1.Klusterlet{
					ObjectMeta: metav1.ObjectMeta{Name: "testmanagedcluster"},
					Status: operatorapiv1.KlusterletStatus{
						Conditions: c.startingConditions,
					},
				},
			)

			hubstatus, updated, err := UpdateClusterManagerStatus(
				context.TODO(),
				fakeOperatorClient.OperatorV1().ClusterManagers(),
				"testmanagedcluster",
				UpdateClusterManagerConditionFn(c.newCondition),
			)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if updated != c.expectedUpdated {
				t.Errorf("expected %t, but %t", c.expectedUpdated, updated)
			}

			klusterletstatus, updated, err := UpdateKlusterletStatus(
				context.TODO(),
				fakeOperatorClient.OperatorV1().Klusterlets(),
				"testmanagedcluster",
				UpdateKlusterletConditionFn(c.newCondition),
			)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if updated != c.expectedUpdated {
				t.Errorf("expected %t, but %t", c.expectedUpdated, updated)
			}

			for i := range c.expectedConditions {
				expected := c.expectedConditions[i]
				hubactual := hubstatus.Conditions[i]
				if expected.LastTransitionTime == (metav1.Time{}) {
					hubactual.LastTransitionTime = metav1.Time{}
				}
				if !equality.Semantic.DeepEqual(expected, hubactual) {
					t.Errorf(diff.ObjectDiff(expected, hubactual))
				}

				klusterletactual := klusterletstatus.Conditions[i]
				if expected.LastTransitionTime == (metav1.Time{}) {
					klusterletactual.LastTransitionTime = metav1.Time{}
				}
				if !equality.Semantic.DeepEqual(expected, klusterletactual) {
					t.Errorf(diff.ObjectDiff(expected, klusterletactual))
				}
			}
		})
	}
}

func newCondition(name, status, reason, message string, lastTransition *metav1.Time) metav1.Condition {
	ret := metav1.Condition{
		Type:    name,
		Status:  metav1.ConditionStatus(status),
		Reason:  reason,
		Message: message,
	}
	if lastTransition != nil {
		ret.LastTransitionTime = *lastTransition
	}
	return ret
}

func newValidatingWebhookConfiguration(name, svc, svcNameSpace string) *admissionv1.ValidatingWebhookConfiguration {
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      svc,
						Namespace: svcNameSpace,
					},
				},
			},
		},
	}
}

func newMutatingWebhookConfiguration(name, svc, svcNameSpace string) *admissionv1.MutatingWebhookConfiguration {
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      svc,
						Namespace: svcNameSpace,
					},
				},
			},
		},
	}
}

func newUnstructured(
	apiVersion, kind, namespace, name string, content map[string]interface{}) *unstructured.Unstructured {
	object := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
		},
	}
	for key, val := range content {
		object.Object[key] = val
	}

	return object
}

func newDeployment(name, namespace string, generation int64) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: generation,
		},
	}
}

func TestApplyValidatingWebhookConfiguration(t *testing.T) {
	testcase := []struct {
		name          string
		existing      []runtime.Object
		expected      *admissionv1.ValidatingWebhookConfiguration
		expectUpdated bool
	}{
		{
			name:          "Create a new configuration",
			expectUpdated: true,
			existing:      []runtime.Object{},
			expected:      newValidatingWebhookConfiguration("test", "svc1", "svc1"),
		},
		{
			name:          "update an existing configuration",
			expectUpdated: true,
			existing:      []runtime.Object{newValidatingWebhookConfiguration("test", "svc1", "svc1")},
			expected:      newValidatingWebhookConfiguration("test", "svc2", "svc2"),
		},
		{
			name:          "skip update",
			expectUpdated: false,
			existing:      []runtime.Object{newValidatingWebhookConfiguration("test", "svc1", "svc1")},
			expected:      newValidatingWebhookConfiguration("test", "svc1", "svc1"),
		},
	}

	for _, c := range testcase {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset(c.existing...)
			_, updated, err := ApplyValidatingWebhookConfiguration(fakeKubeClient.AdmissionregistrationV1(), c.expected)
			if err != nil {
				t.Errorf("Expected no error when applying: %v", err)
			}

			if updated != c.expectUpdated {
				t.Errorf("Expect update is %t, but got %t", c.expectUpdated, updated)
			}
		})
	}
}

func TestApplyMutatingWebhookConfiguration(t *testing.T) {
	testcase := []struct {
		name          string
		existing      []runtime.Object
		expected      *admissionv1.MutatingWebhookConfiguration
		expectUpdated bool
	}{
		{
			name:          "Create a new configuration",
			expectUpdated: true,
			existing:      []runtime.Object{},
			expected:      newMutatingWebhookConfiguration("test", "svc1", "svc1"),
		},
		{
			name:          "update an existing configuration",
			expectUpdated: true,
			existing:      []runtime.Object{newMutatingWebhookConfiguration("test", "svc1", "svc1")},
			expected:      newMutatingWebhookConfiguration("test", "svc2", "svc2"),
		},
		{
			name:          "skip update",
			expectUpdated: false,
			existing:      []runtime.Object{newMutatingWebhookConfiguration("test", "svc1", "svc1")},
			expected:      newMutatingWebhookConfiguration("test", "svc1", "svc1"),
		},
	}

	for _, c := range testcase {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset(c.existing...)
			_, updated, err := ApplyMutatingWebhookConfiguration(fakeKubeClient.AdmissionregistrationV1(), c.expected)
			if err != nil {
				t.Errorf("Expected no error when applying: %v", err)
			}

			if updated != c.expectUpdated {
				t.Errorf("Expect update is %t, but got %t", c.expectUpdated, updated)
			}
		})
	}
}

func TestApplyDirectly(t *testing.T) {
	testcase := []struct {
		name           string
		applyFiles     map[string]runtime.Object
		applyFileNames []string
		expectErr      bool
	}{
		{
			name: "Apply webhooks & apiservice & secret",
			applyFiles: map[string]runtime.Object{
				"validatingwebhooks": newUnstructured("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "", map[string]interface{}{"webhooks": []interface{}{}}),
				"mutatingwebhooks":   newUnstructured("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "", "", map[string]interface{}{"webhooks": []interface{}{}}),
				"apiservice":         newUnstructured("apiregistration.k8s.io/v1", "APIService", "", "", map[string]interface{}{"spec": map[string]interface{}{"service": map[string]string{"name": "svc1", "namespace": "svc1"}}}),
				"secret":             newUnstructured("v1", "Secret", "ns1", "n1", map[string]interface{}{"data": map[string]interface{}{"key1": []byte("key1")}}),
			},
			applyFileNames: []string{"validatingwebhooks", "mutatingwebhooks", "apiservice", "secret"},
			expectErr:      false,
		},
		{
			name: "Apply unhandled object",
			applyFiles: map[string]runtime.Object{
				"kind1": newUnstructured("v1", "Kind1", "ns1", "n1", map[string]interface{}{"spec": map[string]interface{}{"key1": []byte("key1")}}),
			},
			applyFileNames: []string{"kind1"},
			expectErr:      true,
		},
	}

	for _, c := range testcase {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()
			fakeResgistrationClient := fakeapiregistration.NewSimpleClientset()
			fakeExtensionClient := fakeapiextensions.NewSimpleClientset()
			results := ApplyDirectly(
				fakeKubeClient, fakeExtensionClient, fakeResgistrationClient.ApiregistrationV1(),
				eventstesting.NewTestingEventRecorder(t),
				func(name string) ([]byte, error) {
					if c.applyFiles[name] == nil {
						return nil, fmt.Errorf("Failed to find file")
					}

					return json.Marshal(c.applyFiles[name])
				},
				c.applyFileNames...,
			)
			aggregatedErr := []error{}
			for _, r := range results {
				if r.Error != nil {
					aggregatedErr = append(aggregatedErr, r.Error)
				}
			}

			if len(aggregatedErr) == 0 && c.expectErr {
				t.Errorf("Expect an apply error")
			}
			if len(aggregatedErr) != 0 && !c.expectErr {
				t.Errorf("Expect no apply error, %v", operatorhelpers.NewMultiLineAggregate(aggregatedErr))
			}
		})
	}
}

func TestDeleteStaticObject(t *testing.T) {
	applyFiles := map[string]runtime.Object{
		"validatingwebhooks": newUnstructured("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "", map[string]interface{}{"webhooks": []interface{}{}}),
		"mutatingwebhooks":   newUnstructured("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "", "", map[string]interface{}{"webhooks": []interface{}{}}),
		"apiservice":         newUnstructured("apiregistration.k8s.io/v1", "APIService", "", "", map[string]interface{}{"spec": map[string]interface{}{"service": map[string]string{"name": "svc1", "namespace": "svc1"}}}),
		"secret":             newUnstructured("v1", "Secret", "ns1", "n1", map[string]interface{}{"data": map[string]interface{}{"key1": []byte("key1")}}),
		"crd":                newUnstructured("apiextensions.k8s.io/v1beta1", "CustomResourceDefinition", "", "", map[string]interface{}{}),
		"kind1":              newUnstructured("v1", "Kind1", "ns1", "n1", map[string]interface{}{"spec": map[string]interface{}{"key1": []byte("key1")}}),
	}
	testcase := []struct {
		name          string
		applyFileName string
		expectErr     bool
	}{
		{
			name:          "Delete validating webhooks",
			applyFileName: "validatingwebhooks",
			expectErr:     false,
		},
		{
			name:          "Delete mutating webhooks",
			applyFileName: "mutatingwebhooks",
			expectErr:     false,
		},
		{
			name:          "Delete apiservice",
			applyFileName: "apiservice",
			expectErr:     false,
		},
		{
			name:          "Delete secret",
			applyFileName: "secret",
			expectErr:     false,
		},
		{
			name:          "Delete crd",
			applyFileName: "crd",
			expectErr:     false,
		},
		{
			name:          "Delete unhandled object",
			applyFileName: "kind1",
			expectErr:     true,
		},
	}

	for _, c := range testcase {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()
			fakeResgistrationClient := fakeapiregistration.NewSimpleClientset()
			fakeExtensionClient := fakeapiextensions.NewSimpleClientset()
			err := CleanUpStaticObject(
				context.TODO(),
				fakeKubeClient, fakeExtensionClient, fakeResgistrationClient.ApiregistrationV1(),
				func(name string) ([]byte, error) {
					if applyFiles[name] == nil {
						return nil, fmt.Errorf("Failed to find file")
					}

					return json.Marshal(applyFiles[name])
				},
				c.applyFileName,
			)

			if err == nil && c.expectErr {
				t.Errorf("Expect an apply error")
			}
			if err != nil && !c.expectErr {
				t.Errorf("Expect no apply error, %v", err)
			}
		})
	}
}

func TestUpdateGeneration(t *testing.T) {
	gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
	cases := []struct {
		name               string
		startingGeneration []operatorapiv1.GenerationStatus
		newGeneration      operatorapiv1.GenerationStatus
		expectedUpdated    bool
		expectedGeneration []operatorapiv1.GenerationStatus
	}{
		{
			name:               "add to empty",
			startingGeneration: []operatorapiv1.GenerationStatus{},
			newGeneration:      NewGenerationStatus(gvr, newDeployment("test", "test", 0)),
			expectedUpdated:    true,
			expectedGeneration: []operatorapiv1.GenerationStatus{NewGenerationStatus(gvr, newDeployment("test", "test", 0))},
		},
		{
			name: "add to non-conflicting",
			startingGeneration: []operatorapiv1.GenerationStatus{
				NewGenerationStatus(gvr, newDeployment("test", "test", 0)),
			},
			newGeneration:   NewGenerationStatus(gvr, newDeployment("test2", "test", 0)),
			expectedUpdated: true,
			expectedGeneration: []operatorapiv1.GenerationStatus{
				NewGenerationStatus(gvr, newDeployment("test", "test", 0)),
				NewGenerationStatus(gvr, newDeployment("test2", "test", 0)),
			},
		},
		{
			name: "change existing status",
			startingGeneration: []operatorapiv1.GenerationStatus{
				NewGenerationStatus(gvr, newDeployment("test", "test", 0)),
				NewGenerationStatus(gvr, newDeployment("test2", "test", 0)),
			},
			newGeneration:   NewGenerationStatus(gvr, newDeployment("test", "test", 1)),
			expectedUpdated: true,
			expectedGeneration: []operatorapiv1.GenerationStatus{
				NewGenerationStatus(gvr, newDeployment("test", "test", 1)),
				NewGenerationStatus(gvr, newDeployment("test2", "test", 0)),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeOperatorClient := opereatorfake.NewSimpleClientset(
				&operatorapiv1.ClusterManager{
					ObjectMeta: metav1.ObjectMeta{Name: "testmanagedcluster"},
					Status: operatorapiv1.ClusterManagerStatus{
						Generations: c.startingGeneration,
					},
				},
				&operatorapiv1.Klusterlet{
					ObjectMeta: metav1.ObjectMeta{Name: "testmanagedcluster"},
					Status: operatorapiv1.KlusterletStatus{
						Generations: c.startingGeneration,
					},
				},
			)

			hubstatus, updated, err := UpdateClusterManagerStatus(
				context.TODO(),
				fakeOperatorClient.OperatorV1().ClusterManagers(),
				"testmanagedcluster",
				UpdateClusterManagerGenerationsFn(c.newGeneration),
			)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if updated != c.expectedUpdated {
				t.Errorf("expected %t, but %t", c.expectedUpdated, updated)
			}

			klusterletstatus, updated, err := UpdateKlusterletStatus(
				context.TODO(),
				fakeOperatorClient.OperatorV1().Klusterlets(),
				"testmanagedcluster",
				UpdateKlusterletGenerationsFn(c.newGeneration),
			)
			if err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if updated != c.expectedUpdated {
				t.Errorf("expected %t, but %t", c.expectedUpdated, updated)
			}

			for i := range c.expectedGeneration {
				expected := c.expectedGeneration[i]
				hubactual := hubstatus.Generations[i]
				if !equality.Semantic.DeepEqual(expected, hubactual) {
					t.Errorf(diff.ObjectDiff(expected, hubactual))
				}

				klusterletactual := klusterletstatus.Generations[i]
				if !equality.Semantic.DeepEqual(expected, klusterletactual) {
					t.Errorf(diff.ObjectDiff(expected, klusterletactual))
				}
			}
		})
	}
}

func TestLoadClientConfigFromSecret(t *testing.T) {
	testcase := []struct {
		name             string
		secret           *corev1.Secret
		expectedCertData []byte
		expectedKeyData  []byte
		expectedErr      string
	}{
		{
			name:        "load from secret without kubeconfig",
			secret:      newKubeConfigSecret("ns1", "secret1", nil, nil, nil),
			expectedErr: "unable to find kubeconfig in secret \"ns1\" \"secret1\"",
		},
		{
			name:   "load kubeconfig without references to external key/cert files",
			secret: newKubeConfigSecret("ns1", "secret1", newKubeConfig("", ""), nil, nil),
		},
		{
			name:             "load kubeconfig with references to external key/cert files",
			secret:           newKubeConfigSecret("ns1", "secret1", newKubeConfig("tls.crt", "tls.key"), []byte("--- TRUNCATED ---"), []byte("--- REDACTED ---")),
			expectedCertData: []byte("--- TRUNCATED ---"),
			expectedKeyData:  []byte("--- REDACTED ---"),
		},
	}

	for _, c := range testcase {
		t.Run(c.name, func(t *testing.T) {
			config, err := LoadClientConfigFromSecret(c.secret)

			if len(c.expectedErr) > 0 && err == nil {
				t.Errorf("expected %q error", c.expectedErr)
			}

			if len(c.expectedErr) > 0 && err != nil && err.Error() != c.expectedErr {
				t.Errorf("expected %q error, but got %q", c.expectedErr, err.Error())
			}

			if len(c.expectedErr) == 0 && err != nil {
				t.Errorf("unexpected err: %v", err)
			}

			if len(c.expectedCertData) > 0 && !reflect.DeepEqual(c.expectedCertData, config.CertData) {
				t.Errorf("unexpected cert data")
			}

			if len(c.expectedKeyData) > 0 && !reflect.DeepEqual(c.expectedKeyData, config.KeyData) {
				t.Errorf("unexpected key data")
			}
		})
	}
}

func newKubeConfig(certFile, keyFile string) []byte {
	configData, _ := runtime.Encode(clientcmdlatest.Codec, &clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"test-cluster": {
			Server:                "https://test-host:443",
			InsecureSkipTLSVerify: true,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"test-auth": {
			ClientCertificate: certFile,
			ClientKey:         keyFile,
		}},
		Contexts: map[string]*clientcmdapi.Context{"test-context": {
			Cluster:  "test-cluster",
			AuthInfo: "test-auth",
		}},
		CurrentContext: "test-context",
	})
	return configData
}

func newKubeConfigSecret(namespace, name string, kubeConfigData, certData, keyData []byte) *corev1.Secret {
	data := map[string][]byte{}
	if len(kubeConfigData) > 0 {
		data["kubeconfig"] = kubeConfigData
	}
	if len(certData) > 0 {
		data["tls.crt"] = certData
	}
	if len(keyData) > 0 {
		data["tls.key"] = keyData
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: data,
	}
}

func TestDeterminReplica(t *testing.T) {
	cases := []struct {
		name            string
		existingNodes   []runtime.Object
		expectedReplica int32
	}{
		{
			name:            "single node",
			existingNodes:   []runtime.Object{newNode("node1")},
			expectedReplica: singleReplica,
		},
		{
			name:            "multiple node",
			existingNodes:   []runtime.Object{newNode("node1"), newNode("node2"), newNode("node3")},
			expectedReplica: defaultReplica,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset(c.existingNodes...)
			replica := DetermineReplicaByNodes(context.Background(), fakeKubeClient)
			if replica != c.expectedReplica {
				t.Errorf("Unexpected replica, actual: %d, expected: %d", replica, c.expectedReplica)
			}
		})
	}
}

func newNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"node-role.kubernetes.io/master": "",
			},
		},
	}
}

func newDeploymentUnstructured(name, namespace string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []map[string]interface{}{
						{
							"name":  "hub-registration-controller",
							"image": "quay.io/open-cluster-management/registration:latest",
						},
					},
				}}}}

	return newUnstructured("apps/v1", "Deployment", namespace, name, spec)
}

func TestApplyDeployment(t *testing.T) {
	testcases := []struct {
		name                string
		deploymentName      string
		deploymentNamespace string
		nodePlacement       operatorapiv1.NodePlacement
		expectErr           bool
	}{
		{
			name:                "Apply a deployment without nodePlacement",
			deploymentName:      "cluster-manager-registration-controller",
			deploymentNamespace: "open-cluster-management-hub",
			expectErr:           false,
		},
		{
			name:                "Apply a deployment with nodePlacement",
			deploymentName:      "cluster-manager-registration-controller",
			deploymentNamespace: "open-cluster-management-hub",
			nodePlacement: operatorapiv1.NodePlacement{
				NodeSelector: map[string]string{"node-role.kubernetes.io/infra": ""},
				Tolerations: []corev1.Toleration{
					{
						Key:      "node-role.kubernetes.io/infra",
						Operator: corev1.TolerationOpExists,
						Effect:   corev1.TaintEffectNoSchedule,
					},
				},
			},
			expectErr: false,
		},
	}

	for _, c := range testcases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()
			_, err := ApplyDeployment(
				fakeKubeClient, []operatorapiv1.GenerationStatus{}, c.nodePlacement,
				func(name string) ([]byte, error) {
					return json.Marshal(newDeploymentUnstructured(c.deploymentName, c.deploymentNamespace))
				},
				eventstesting.NewTestingEventRecorder(t),
				c.deploymentName,
			)
			if err != nil && !c.expectErr {
				t.Errorf("Expect an apply error")
			}

			deployment, err := fakeKubeClient.AppsV1().Deployments(c.deploymentNamespace).Get(context.TODO(), c.deploymentName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("Expect an get error")
			}

			if !reflect.DeepEqual(deployment.Spec.Template.Spec.NodeSelector, c.nodePlacement.NodeSelector) {
				t.Errorf("Expect nodeSelector %v, got %v", c.nodePlacement.NodeSelector, deployment.Spec.Template.Spec.NodeSelector)
			}
			if !reflect.DeepEqual(deployment.Spec.Template.Spec.Tolerations, c.nodePlacement.Tolerations) {
				t.Errorf("Expect Tolerations %v, got %v", c.nodePlacement.Tolerations, deployment.Spec.Template.Spec.Tolerations)
			}
		})
	}
}
