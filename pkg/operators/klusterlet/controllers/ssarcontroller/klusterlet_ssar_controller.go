package ssarcontroller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreinformer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	operatorv1client "open-cluster-management.io/api/client/operator/clientset/versioned/typed/operator/v1"
	operatorinformer "open-cluster-management.io/api/client/operator/informers/externalversions/operator/v1"
	operatorlister "open-cluster-management.io/api/client/operator/listers/operator/v1"
	"open-cluster-management.io/registration-operator/pkg/helpers"
)

// SSARReSyncTime is exposed so that integration tests can crank up the controller sync speed.
var SSARReSyncTime = 30 * time.Second

type ssarController struct {
	kubeClient       kubernetes.Interface
	secretLister     corelister.SecretLister
	klusterletClient operatorv1client.KlusterletInterface
	klusterletLister operatorlister.KlusterletLister
	*klusterletLocker
}

type klusterletLocker struct {
	sync.RWMutex
	klusterletInChecking map[string]struct{}
}

const (
	klusterletNamespace     = "open-cluster-management-agent"
	bootstrapSecret         = "BootstrapSecret"
	bootstrapSecretDegraded = "BootstrapSecretDegraded"
	hubConfigSecret         = "HubConfigSecret"
	hubConfigSecretDegraded = "HubConfigSecretDegraded"
	hubConnectionDegraded   = "HubConnectionDegraded"
)

func NewKlustrletSSARController(
	kubeClient kubernetes.Interface,
	klusterletClient operatorv1client.KlusterletInterface,
	klusterletInformer operatorinformer.KlusterletInformer,
	secretInformer coreinformer.SecretInformer,
	recorder events.Recorder,
) factory.Controller {
	controller := &ssarController{
		kubeClient:       kubeClient,
		klusterletClient: klusterletClient,
		klusterletLister: klusterletInformer.Lister(),
		secretLister:     secretInformer.Lister(),
		klusterletLocker: &klusterletLocker{
			klusterletInChecking: make(map[string]struct{}),
		},
	}

	return factory.New().WithSync(controller.sync).
		WithInformersQueueKeyFunc(helpers.KlusterletSecretQueueKeyFunc(controller.klusterletLister), secretInformer.Informer()).
		WithInformersQueueKeyFunc(func(obj runtime.Object) string {
			accessor, _ := meta.Accessor(obj)
			return accessor.GetName()
		}, klusterletInformer.Informer()).
		ToController("KlusterletSSARController", recorder)
}

func (l *klusterletLocker) inSSARChecking(klusterletName string) bool {
	l.RLock()
	defer l.RUnlock()
	_, ok := l.klusterletInChecking[klusterletName]
	return ok
}

func (l *klusterletLocker) addSSARChecking(klusterletName string) {
	l.Lock()
	defer l.Unlock()
	l.klusterletInChecking[klusterletName] = struct{}{}
}

func (l *klusterletLocker) deleteSSARChecking(klusterletName string) {
	l.Lock()
	defer l.Unlock()
	delete(l.klusterletInChecking, klusterletName)
}

func (c *ssarController) sync(ctx context.Context, controllerContext factory.SyncContext) error {
	klusterletName := controllerContext.QueueKey()
	if klusterletName == "" {
		return nil
	}
	klusterlet, err := c.klusterletLister.Get(klusterletName)
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	}
	klusterlet = klusterlet.DeepCopy()

	// if the ssar checking is already processing, requeue it after 30s.
	if c.inSSARChecking(klusterletName) {
		klog.V(4).Infof("Reconciling Klusterlet %q is already processing now", klusterletName)
		controllerContext.Queue().AddAfter(klusterletName, SSARReSyncTime)
		return nil
	}

	c.addSSARChecking(klusterletName)
	go func() {
		defer c.deleteSSARChecking(klusterletName)

		klog.V(4).Infof("Reconciling Klusterlet %q", klusterletName)
		klusterletNS := klusterlet.Spec.Namespace
		if klusterletNS == "" {
			klusterletNS = klusterletNamespace
		}

		hubConfigDegradedCondition := checkAgentDegradedCondition(
			ctx, c.kubeClient,
			hubConfigSecret, hubConfigSecretDegraded,
			klusterletAgent{
				clusterName: klusterlet.Spec.ClusterName,
				namespace:   klusterletNS,
			},
			[]degradedCheckFunc{checkHubConfigSecret},
		)

		// the hub kubeconfig is functional, the bootstrap kubeconfig check is not needed,
		// ignore it to reduce to send the sar requests
		if hubConfigDegradedCondition.Status == metav1.ConditionFalse {
			_, _, err := helpers.UpdateKlusterletStatus(
				ctx,
				c.klusterletClient,
				klusterletName,
				helpers.UpdateKlusterletConditionFn(metav1.Condition{
					Type:    hubConnectionDegraded,
					Status:  metav1.ConditionFalse,
					Reason:  fmt.Sprintf("HubConnectionFunctional"),
					Message: fmt.Sprintf("Hub connection is functioning correctly"),
				}),
			)

			if err != nil {
				klog.Errorf("Update Klusterlet Status Failed: %v", err)
				controllerContext.Queue().AddAfter(klusterletName, SSARReSyncTime)
			}

			return
		}

		bootstrapDegradedCondition := checkAgentDegradedCondition(
			ctx, c.kubeClient,
			bootstrapSecret, bootstrapSecretDegraded,
			klusterletAgent{
				clusterName: klusterlet.Spec.ClusterName,
				namespace:   klusterletNS,
			},
			[]degradedCheckFunc{checkBootstrapSecret},
		)

		// merge the bootstrap and hubconfig degraded conditions to hub connection degraged condition
		_, _, err := helpers.UpdateKlusterletStatus(
			ctx,
			c.klusterletClient,
			klusterletName,
			helpers.UpdateKlusterletConditionFn(metav1.Condition{
				Type:    hubConnectionDegraded,
				Status:  metav1.ConditionTrue,
				Reason:  bootstrapDegradedCondition.Reason + "," + hubConfigDegradedCondition.Reason,
				Message: bootstrapDegradedCondition.Message + "\n" + hubConfigDegradedCondition.Message,
			}),
		)
		if err != nil {
			klog.Errorf("Update Klusterlet Status Failed: %v", err)
			controllerContext.Queue().AddAfter(klusterletName, SSARReSyncTime)
		}
	}()

	return nil
}

type klusterletAgent struct {
	clusterName string
	namespace   string
}

func checkAgentDegradedCondition(
	ctx context.Context, kubeClient kubernetes.Interface,
	secretName, degradedType string,
	agent klusterletAgent,
	degradedCheckFns []degradedCheckFunc) metav1.Condition {
	degradedConditionReasons := []string{}
	degradedConditionMessages := []string{}
	for _, degradedCheckFn := range degradedCheckFns {
		currCond := degradedCheckFn(ctx, kubeClient, agent)
		if currCond == nil {
			continue
		}
		degradedConditionReasons = append(degradedConditionReasons, currCond.Reason)
		degradedConditionMessages = append(degradedConditionMessages, currCond.Message)
	}

	if len(degradedConditionReasons) == 0 {
		return metav1.Condition{
			Type:    degradedType,
			Status:  metav1.ConditionFalse,
			Reason:  fmt.Sprintf("%sFunctional", secretName),
			Message: fmt.Sprintf("%s is functioning correctly", secretName),
		}
	}

	return metav1.Condition{
		Type:    degradedType,
		Status:  metav1.ConditionTrue,
		Reason:  strings.Join(degradedConditionReasons, ","),
		Message: strings.Join(degradedConditionMessages, "\n"),
	}
}

type degradedCheckFunc func(ctx context.Context, kubeClient kubernetes.Interface, agent klusterletAgent) *metav1.Condition

// Check bootstrap secret, if the secret is invalid, return registration degraded condition
func checkBootstrapSecret(ctx context.Context, kubeClient kubernetes.Interface, agent klusterletAgent) *metav1.Condition {
	// Check if bootstrap secret exists
	bootstrapSecret, err := kubeClient.CoreV1().Secrets(agent.namespace).Get(ctx, helpers.BootstrapHubKubeConfig, metav1.GetOptions{})
	if err != nil {
		return &metav1.Condition{
			Reason:  "BootstrapSecretMissing",
			Message: fmt.Sprintf("Failed to get bootstrap secret %q %q: %v", agent.namespace, helpers.BootstrapHubKubeConfig, err),
		}
	}

	// Check if bootstrap secret works by building kube client
	bootstrapClient, err := buildKubeClientWithSecret(bootstrapSecret)
	if err != nil {
		return &metav1.Condition{
			Reason: "BootstrapSecretError",
			Message: fmt.Sprintf("Failed to build bootstrap kube client with bootstrap secret %q %q: %v",
				agent.namespace, helpers.BootstrapHubKubeConfig, err),
		}
	}

	// Check the bootstrap client permissions by creating SelfSubjectAccessReviews
	allowed, failedReview, err := createSelfSubjectAccessReviews(ctx, bootstrapClient, getBootstrapSSARs())
	if err != nil {
		return &metav1.Condition{
			Reason: "BootstrapSecretError",
			Message: fmt.Sprintf("Failed to create %+v with bootstrap secret %q %q: %v",
				failedReview, agent.namespace, helpers.BootstrapHubKubeConfig, err),
		}
	}
	if !allowed {
		return &metav1.Condition{
			Reason: "BootstrapSecretUnauthorized",
			Message: fmt.Sprintf("Operation for resource %+v is not allowed with bootstrap secret %q %q",
				failedReview.Spec.ResourceAttributes, agent.namespace, helpers.BootstrapHubKubeConfig),
		}
	}

	return nil
}

func getBootstrapSSARs() []authorizationv1.SelfSubjectAccessReview {
	reviews := []authorizationv1.SelfSubjectAccessReview{}
	clusterResource := authorizationv1.ResourceAttributes{
		Group:    "cluster.open-cluster-management.io",
		Resource: "managedclusters",
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(clusterResource, "create", "get")...)

	certResource := authorizationv1.ResourceAttributes{
		Group:    "certificates.k8s.io",
		Resource: "certificatesigningrequests",
	}
	return append(reviews, generateSelfSubjectAccessReviews(certResource, "create", "get", "list", "watch")...)
}

// Check hub-kubeconfig-secret, if the secret is invalid, return degraded condition
func checkHubConfigSecret(ctx context.Context, kubeClient kubernetes.Interface, agent klusterletAgent) *metav1.Condition {
	hubConfigSecret, err := kubeClient.CoreV1().Secrets(agent.namespace).Get(ctx, helpers.HubKubeConfig, metav1.GetOptions{})
	if err != nil {
		return &metav1.Condition{
			Reason:  "HubKubeConfigSecretMissing",
			Message: fmt.Sprintf("Failed to get hub kubeconfig secret %q %q: %v", agent.namespace, helpers.HubKubeConfig, err),
		}
	}

	if hubConfigSecret.Data["kubeconfig"] == nil {
		return &metav1.Condition{
			Reason: "HubKubeConfigMissing",
			Message: fmt.Sprintf("Failed to get kubeconfig from `kubectl get secret -n %q %q -ojsonpath='{.data.kubeconfig}'`. "+
				"This is set by the klusterlet registration deployment, but the CSR must be approved by the cluster-admin on the hub.",
				hubConfigSecret.Namespace, hubConfigSecret.Name),
		}
	}

	hubClient, err := buildKubeClientWithSecret(hubConfigSecret)
	if err != nil {
		return &metav1.Condition{
			Reason: "HubKubeConfigError",
			Message: fmt.Sprintf("Failed to build hub kube client with hub config secret %q %q: %v",
				hubConfigSecret.Namespace, hubConfigSecret.Name, err),
		}
	}

	clusterName := agent.clusterName
	// If cluster name is empty, read cluster name from hub config secret
	if clusterName == "" {
		if hubConfigSecret.Data["cluster-name"] == nil {
			return &metav1.Condition{
				Reason: "ClusterNameMissing",
				Message: fmt.Sprintf(
					"Failed to get cluster name from `kubectl get secret -n %q %q -ojsonpath='{.data.cluster-name}`."+
						" This is set by the klusterlet registration deployment.", hubConfigSecret.Namespace, hubConfigSecret.Name),
			}
		}
		clusterName = string(hubConfigSecret.Data["cluster-name"])
	}

	// Check the hub kubeconfig permissions by creating SelfSubjectAccessReviews
	allowed, failedReview, err := createSelfSubjectAccessReviews(ctx, hubClient, getHubConfigSSARs(clusterName))
	if err != nil {
		return &metav1.Condition{
			Reason: "HubKubeConfigError",
			Message: fmt.Sprintf("Failed to create %+v with hub config secret %q %q: %v",
				failedReview, hubConfigSecret.Namespace, hubConfigSecret.Name, err),
		}
	}
	if !allowed {
		return &metav1.Condition{
			Reason: "HubKubeConfigUnauthorized",
			Message: fmt.Sprintf("Operation for resource %+v is not allowed with hub config secret %q %q",
				failedReview.Spec.ResourceAttributes, hubConfigSecret.Namespace, hubConfigSecret.Name),
		}
	}

	return nil
}

func getHubConfigSSARs(clusterName string) []authorizationv1.SelfSubjectAccessReview {
	reviews := []authorizationv1.SelfSubjectAccessReview{}

	// registration resources
	certResource := authorizationv1.ResourceAttributes{
		Group:    "certificates.k8s.io",
		Resource: "certificatesigningrequests",
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(certResource, "get", "list", "watch")...)

	clusterResource := authorizationv1.ResourceAttributes{
		Group:    "cluster.open-cluster-management.io",
		Resource: "managedclusters",
		Name:     clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(clusterResource, "get", "list", "update", "watch")...)

	clusterStatusResource := authorizationv1.ResourceAttributes{
		Group:       "cluster.open-cluster-management.io",
		Resource:    "managedclusters",
		Subresource: "status",
		Name:        clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(clusterStatusResource, "patch", "update")...)

	clusterCertResource := authorizationv1.ResourceAttributes{
		Group:       "register.open-cluster-management.io",
		Resource:    "managedclusters",
		Subresource: "clientcertificates",
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(clusterCertResource, "renew")...)

	leaseResource := authorizationv1.ResourceAttributes{
		Group:     "coordination.k8s.io",
		Resource:  "leases",
		Name:      fmt.Sprintf("cluster-lease-%s", clusterName),
		Namespace: clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(leaseResource, "get", "update")...)

	// work resources
	eventResource := authorizationv1.ResourceAttributes{
		Resource:  "events",
		Namespace: clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(eventResource, "create", "patch", "update")...)

	eventResource = authorizationv1.ResourceAttributes{
		Group:     "events.k8s.io",
		Resource:  "events",
		Namespace: clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(eventResource, "create", "patch", "update")...)

	workResource := authorizationv1.ResourceAttributes{
		Group:     "work.open-cluster-management.io",
		Resource:  "manifestworks",
		Namespace: clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(workResource, "get", "list", "watch", "update")...)

	workStatusResource := authorizationv1.ResourceAttributes{
		Group:       "work.open-cluster-management.io",
		Resource:    "manifestworks",
		Subresource: "status",
		Namespace:   clusterName,
	}
	reviews = append(reviews, generateSelfSubjectAccessReviews(workStatusResource, "patch", "update")...)
	return reviews
}

func buildKubeClientWithSecret(secret *corev1.Secret) (kubernetes.Interface, error) {
	restConfig, err := helpers.LoadClientConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}

	// reduce qps and burst of client, because too many managed clusters registration on hub and send ssar requests at once could cause resource pressure
	restConfig.QPS = 2
	restConfig.Burst = 5

	return kubernetes.NewForConfig(restConfig)
}

func generateSelfSubjectAccessReviews(resource authorizationv1.ResourceAttributes, verbs ...string) []authorizationv1.SelfSubjectAccessReview {
	reviews := []authorizationv1.SelfSubjectAccessReview{}
	for _, verb := range verbs {
		reviews = append(reviews, authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group:       resource.Group,
					Resource:    resource.Resource,
					Subresource: resource.Subresource,
					Name:        resource.Name,
					Namespace:   resource.Namespace,
					Verb:        verb,
				},
			},
		})
	}
	return reviews
}

func createSelfSubjectAccessReviews(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	selfSubjectAccessReviews []authorizationv1.SelfSubjectAccessReview) (bool, *authorizationv1.SelfSubjectAccessReview, error) {

	for i := range selfSubjectAccessReviews {
		subjectAccessReview := selfSubjectAccessReviews[i]

		ssar, err := kubeClient.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &subjectAccessReview, metav1.CreateOptions{})
		if err != nil {
			return false, &subjectAccessReview, err
		}
		if !ssar.Status.Allowed {
			return false, &subjectAccessReview, nil
		}
	}
	return true, nil, nil
}
