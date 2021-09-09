package certificate

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-cluster-management/addon-framework/pkg/addonmanager/constants"
	"github.com/open-cluster-management/addon-framework/pkg/agent"
	addoninformerv1alpha1 "github.com/open-cluster-management/api/client/addon/informers/externalversions/addon/v1alpha1"
	addonlisterv1alpha1 "github.com/open-cluster-management/api/client/addon/listers/addon/v1alpha1"
	clusterinformers "github.com/open-cluster-management/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlister "github.com/open-cluster-management/api/client/cluster/listers/cluster/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	certificatesinformers "k8s.io/client-go/informers/certificates/v1"
	"k8s.io/client-go/kubernetes"
	certificateslisters "k8s.io/client-go/listers/certificates/v1"
	"k8s.io/klog/v2"
)

// csrApprovingController auto approve the renewal CertificateSigningRequests for an accepted spoke cluster on the hub.
type csrSignController struct {
	kubeClient                kubernetes.Interface
	agentAddons               map[string]agent.AgentAddon
	eventRecorder             events.Recorder
	managedClusterLister      clusterlister.ManagedClusterLister
	managedClusterAddonLister addonlisterv1alpha1.ManagedClusterAddOnLister
	csrLister                 certificateslisters.CertificateSigningRequestLister
}

// NewCSRApprovingController creates a new csr approving controller
func NewCSRSignController(
	kubeClient kubernetes.Interface,
	clusterInformers clusterinformers.ManagedClusterInformer,
	csrInformer certificatesinformers.CertificateSigningRequestInformer,
	addonInformers addoninformerv1alpha1.ManagedClusterAddOnInformer,
	agentAddons map[string]agent.AgentAddon,
	recorder events.Recorder,
) factory.Controller {
	c := &csrSignController{
		kubeClient:                kubeClient,
		agentAddons:               agentAddons,
		managedClusterLister:      clusterInformers.Lister(),
		managedClusterAddonLister: addonInformers.Lister(),
		csrLister:                 csrInformer.Lister(),
		eventRecorder:             recorder.WithComponentSuffix(fmt.Sprintf("csr-signing-controller")),
	}
	return factory.New().
		WithFilteredEventsInformersQueueKeyFunc(
			func(obj runtime.Object) string {
				accessor, _ := meta.Accessor(obj)
				return accessor.GetName()
			},
			func(obj interface{}) bool {
				accessor, _ := meta.Accessor(obj)
				if !strings.HasPrefix(accessor.GetName(), "addon") {
					return false
				}
				if len(accessor.GetLabels()) == 0 {
					return false
				}
				addonName := accessor.GetLabels()[constants.AddonLabel]
				if _, ok := agentAddons[addonName]; !ok {
					return false
				}
				return true
			},
			csrInformer.Informer()).
		WithSync(c.sync).
		ToController("CSRApprovingController", recorder)
}

func (c *csrSignController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	csrName := syncCtx.QueueKey()
	klog.Infof("++++>CSRSignController(%s)", csrName)
	csr, err := c.csrLister.Get(csrName)
	if errors.IsNotFound(err) {
		klog.Infof("++++>CSRSignController(%s): exit 0", csrName)
		return nil
	}
	if err != nil {
		klog.Infof("++++>CSRSignController(%s): exit 1", csrName)
		return err
	}
	csr = csr.DeepCopy()

	if !isCSRApproved(csr) {
		klog.Infof("++++>CSRSignController(%s): exit 2", csrName)
		return nil
	}

	if len(csr.Status.Certificate) > 0 {
		klog.Infof("++++>CSRSignController(%s): exit 3", csrName)
		return nil
	}

	// Do not sigh apiserver cert
	if csr.Spec.SignerName == certificatesv1.KubeAPIServerClientSignerName {
		klog.Infof("++++>CSRSignController(%s): exit 4", csrName)
		return nil
	}

	addonName := csr.Labels[constants.AddonLabel]
	agentAddon, ok := c.agentAddons[addonName]
	if !ok {
		klog.Infof("++++>CSRSignController(%s): exit 5", csrName)
		return nil
	}

	registrationOption := agentAddon.GetAgentAddonOptions().Registration
	if registrationOption == nil {
		klog.Infof("++++>CSRSignController(%s): exit 6", csrName)
		return nil
	}
	clusterName, ok := csr.Labels[constants.ClusterLabel]
	if !ok {
		klog.Infof("++++>CSRSignController(%s): exit 7", csrName)
		return nil
	}

	// Get ManagedCluster
	_, err = c.managedClusterLister.Get(clusterName)
	if errors.IsNotFound(err) {
		klog.Infof("++++>CSRSignController(%s): exit 8", csrName)
		return nil
	}
	if err != nil {
		klog.Infof("++++>CSRSignController(%s): exit 9", csrName)
		return err
	}

	_, err = c.managedClusterAddonLister.ManagedClusterAddOns(clusterName).Get(addonName)
	if errors.IsNotFound(err) {
		klog.Infof("++++>CSRSignController(%s): exit 10", csrName)
		return nil
	}
	if err != nil {
		klog.Infof("++++>CSRSignController(%s): exit 11", csrName)
		return err
	}

	if registrationOption.CSRSign == nil {
		klog.Infof("++++>CSRSignController(%s): exit 12", csrName)
		return nil
	}

	csr.Status.Certificate = registrationOption.CSRSign(csr)

	klog.Infof("++++>CSRSignController(%s): len(cert)=%d", csrName, len(csr.Status.Certificate))

	_, err = c.kubeClient.CertificatesV1().CertificateSigningRequests().UpdateStatus(ctx, csr, metav1.UpdateOptions{})
	if err != nil {
		klog.Infof("++++>CSRSignController(%s): exit 13", csrName)
		return err
	}
	c.eventRecorder.Eventf("AddonCSRAutoApproved", "addon csr %q is signedr", csr.Name)
	klog.Infof("++++>CSRSignController(%s): exit 14", csrName)
	return nil
}
