/*
Copyright 2021 The cert-manager Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apiutil "github.com/cert-manager/cert-manager/pkg/api/util"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	cmversioned "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	cmclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned/typed/certmanager/v1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	identityAnnotation = "istio.cert-manager.io/identities"
)

type Options struct {
	// If PreserveCertificateRequests is true, requests will not be deleted after
	// they are signed.
	PreserveCertificateRequests bool

	// Namespace is the namespace that CertificateRequests will be created in.
	Namespace string

	// IssuerRef is used as the issuerRef on created CertificateRequests.
	IssuerRef cmmeta.ObjectReference

	// IssuanceConfigMapName is the name of a ConfigMap to watch for configuration options. The ConfigMap is expected to be in the same namespace as the csi-driver-spiffe pod.
	IssuanceConfigMapName string

	// IssuanceConfigMapNamespace is the namespace where the runtime configuration ConfigMap is located
	IssuanceConfigMapNamespace string

	// AdditionalAnnotations are any additional annotations to include on created CertificateRequests.
	AdditionalAnnotations map[string]string
}

func (o Options) HasRuntimeConfiguration() bool {
	return o.IssuanceConfigMapName != "" && o.IssuanceConfigMapNamespace != ""
}

type Signer interface {
	// Sign will create a CertificateRequest based on the provided inputs. It will
	// wait for it to reach a terminal state, before optionally deleting it if
	// preserving CertificateRequests if turned off. Will return the certificate
	// bundle on successful signing.
	Sign(ctx context.Context, identities string, csrPEM []byte, duration time.Duration, usages []cmapi.KeyUsage) (Bundle, error)
}

// manager is used for signing CSRs via cert-manager. manager will create
// CertificateRequests and wait for them to be signed, before returning the
// result.
type manager struct {
	opts Options
	log  logr.Logger

	// kubernetesClient is used to watch ConfigMaps for issuance configuration
	kubernetesClient client.WithWatch

	certManagerClient cmclient.CertificateRequestInterface

	// activeIssuerRef controls the issuerRef actually used when creating
	// CertificateRequest objects. Can be empty, which will cause issuance to
	// fail until runtime configuration is applied.
	activeIssuerRef *cmmeta.ObjectReference

	activeIssuerRefMutex sync.RWMutex

	// originalIssuerRef is the issuerRef passed at startup. This will be used
	// if no runtime configuration (ConfigMap configuration) is found, or if the
	// ConfigMap for runtime configuration is deleted.
	originalIssuerRef *cmmeta.ObjectReference
}

// Bundle represents the `status.Certificate` and `status.CA` that is
// populate on a CertificateRequest once it has been signed.
type Bundle struct {
	Certificate []byte
	CA          []byte
}

func New(log logr.Logger, restConfig *rest.Config, opts Options) (*manager, error) {
	k8sClient, err := client.NewWithWatch(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("failed to build kubernetes watcher client: %w", err)
	}

	cmClient, err := cmversioned.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build cert-manager client: %s", err)
	}

	originalIssuerRef, err := handleOriginalIssuerRef(opts.IssuerRef)
	if err != nil && err != errNoOriginalIssuer {
		return nil, fmt.Errorf("invalid issuerRef passed at startup: %s", err)
	}

	activeIssuerRef := originalIssuerRef

	if err == errNoOriginalIssuer {
		if !opts.HasRuntimeConfiguration() {
			return nil, fmt.Errorf("runtime configuration parameters for name and namespace are required if no issuerRef is provided on startup")
		}

		activeIssuerRef = nil
	}

	return &manager{
		log: log.WithName("cert-manager"),

		kubernetesClient:  k8sClient,
		certManagerClient: cmClient.CertmanagerV1().CertificateRequests(opts.Namespace),
		opts:              opts,

		activeIssuerRef: activeIssuerRef,

		originalIssuerRef: originalIssuerRef,
	}, nil
}

// Sign will sign a request against the manager's configured client.
func (m *manager) Sign(ctx context.Context, identities string, csrPEM []byte, duration time.Duration, usages []cmapi.KeyUsage) (Bundle, error) {
	m.activeIssuerRefMutex.RLock()
	defer m.activeIssuerRefMutex.RUnlock()

	if m.activeIssuerRef == nil {
		return Bundle{}, fmt.Errorf("no active issuerRef is configured for istio-csr")
	}

	cr := &cmapi.CertificateRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "istio-csr-",
			Annotations: map[string]string{
				identityAnnotation: identities,
			},
		},
		Spec: cmapi.CertificateRequestSpec{
			Duration: &metav1.Duration{
				Duration: duration,
			},
			IsCA:      false,
			Request:   csrPEM,
			Usages:    usages,
			IssuerRef: *m.activeIssuerRef,
		},
	}

	for k, v := range m.opts.AdditionalAnnotations {
		cr.ObjectMeta.Annotations[k] = v
	}
	// Create CertificateRequest and wait for it to be successfully signed.
	cr, err := m.certManagerClient.Create(ctx, cr, metav1.CreateOptions{})
	if err != nil {
		return Bundle{}, fmt.Errorf("failed to create CertificateRequest: %w", err)
	}

	log := m.log.WithValues("namespace", cr.Namespace, "name", cr.Name, "identity", identities)
	log.V(2).Info("created CertificateRequest")

	// If we are not preserving CertificateRequests, always delete from
	// Kubernetes on return.
	if !m.opts.PreserveCertificateRequests {
		// nolint:contextcheck
		defer func() {
			// Use go routine to prevent blocking on Delete call.
			go func() {
				// Use the Background context so that this call is not cancelled by the
				// gRPC context closing.
				cleanupCtx := context.Background()

				if err := m.certManagerClient.Delete(cleanupCtx, cr.Name, metav1.DeleteOptions{}); err != nil {
					log.Error(err, "failed to delete CertificateRequest")
					return
				}

				log.V(2).Info("deleted CertificateRequest")
			}()
		}()
	}

	signedCR, err := m.waitForCertificateRequest(ctx, log, cr)
	if err != nil {
		return Bundle{}, fmt.Errorf("failed to wait for CertificateRequest %s/%s to be signed: %w",
			cr.Namespace, cr.Name, err)
	}

	log.V(2).Info("signed CertificateRequest")

	return Bundle{Certificate: signedCR.Status.Certificate, CA: signedCR.Status.CA}, nil
}

// waitForCertificateRequest will set a watch for the CertificateRequest, and
// will return the CertificateRequest once it has reached a terminal state. If
// the terminal state is either Denied or Failed, then this will also return an
// error.
func (m *manager) waitForCertificateRequest(ctx context.Context, log logr.Logger, cr *cmapi.CertificateRequest) (*cmapi.CertificateRequest, error) {
	watcher, err := m.certManagerClient.Watch(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(metav1.ObjectNameField, cr.Name).String(),
	})
	if err != nil {
		return cr, fmt.Errorf("failed to build watcher for CertificateRequest: %w", err)
	}
	defer watcher.Stop()

	// Get the request in-case it has already reached a terminal state.
	cr, err = m.certManagerClient.Get(ctx, cr.Name, metav1.GetOptions{})
	if err != nil {
		return cr, fmt.Errorf("failed to get CertificateRequest: %w", err)
	}

	for {
		if apiutil.CertificateRequestIsDenied(cr) {
			return cr, fmt.Errorf("created CertificateRequest has been denied: %v", cr.Status.Conditions)
		}

		if apiutil.CertificateRequestHasCondition(cr, cmapi.CertificateRequestCondition{
			Type:   cmapi.CertificateRequestConditionReady,
			Status: cmmeta.ConditionFalse,
			Reason: cmapi.CertificateRequestReasonFailed,
		}) {
			return cr, fmt.Errorf("created CertificateRequest has failed: %v", cr.Status.Conditions)
		}

		if len(cr.Status.Certificate) > 0 {
			return cr, nil
		}

		log.V(3).Info("waiting for CertificateRequest to become ready")

		for {
			w, ok := <-watcher.ResultChan()
			if !ok {
				return cr, errors.New("watcher channel closed")
			}
			if w.Type == watch.Deleted {
				return cr, errors.New("created CertificateRequest has been unexpectedly deleted")
			}

			cr, ok = w.Object.(*cmapi.CertificateRequest)
			if !ok {
				log.Error(nil, "got unexpected object response from watcher", "object", w.Object)
				continue
			}
			break
		}
	}
}

const (
	issuerNameKey  = "issuer-name"
	issuerKindKey  = "issuer-kind"
	issuerGroupKey = "issuer-group"
)

func (m *manager) handleRuntimeConfigIssuerChange(logger logr.Logger, event watch.Event) error {
	m.activeIssuerRefMutex.Lock()
	defer m.activeIssuerRefMutex.Unlock()

	cm, ok := event.Object.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("got unexpected type for runtime configuration source; this is likely a programming error")
	}

	issuerRef := &cmmeta.ObjectReference{}

	var dataErrs []error
	var exists bool

	issuerRef.Name, exists = cm.Data[issuerNameKey]
	if !exists || len(issuerRef.Name) == 0 {
		dataErrs = append(dataErrs, fmt.Errorf("missing key/value in ConfigMap data: %s", issuerNameKey))
	}

	issuerRef.Kind, exists = cm.Data[issuerKindKey]
	if !exists || len(issuerRef.Kind) == 0 {
		dataErrs = append(dataErrs, fmt.Errorf("missing key/value in ConfigMap data: %s", issuerKindKey))
	}

	issuerRef.Group, exists = cm.Data[issuerGroupKey]
	if !exists || len(issuerRef.Group) == 0 {
		dataErrs = append(dataErrs, fmt.Errorf("missing key/value in ConfigMap data; %s", issuerGroupKey))
	}

	if len(dataErrs) > 0 {
		return errors.Join(dataErrs...)
	}

	// we now have a full issuerRef
	// TODO: check if the issuer exists by querying for the CRD?

	m.activeIssuerRef = issuerRef

	logger.Info("Changed active issuerRef in response to runtime configuration ConfigMap", "issuer-name", m.activeIssuerRef.Name, "issuer-kind", m.activeIssuerRef.Kind, "issuer-group", m.activeIssuerRef.Group)

	return nil
}

func (m *manager) handleRuntimeConfigIssuerDeletion(logger logr.Logger) {
	m.activeIssuerRefMutex.Lock()
	defer m.activeIssuerRefMutex.Unlock()

	if m.originalIssuerRef == nil {
		logger.Info("Runtime issuance configuration was deleted and no issuerRef was configured at install time; issuance will fail until runtime configuration is reinstated")
		m.activeIssuerRef = nil
		return
	}

	logger.Info("Runtime issuance configuration was deleted; issuance will revert to original issuerRef configured at install time")

	m.activeIssuerRef = m.originalIssuerRef
}

// RuntimeConfigurationWatcher is a wrapper around ctrlmgr.Runnable for watching runtime config
type RuntimeConfigurationWatcher struct {
	m *manager
}

// NeedLeaderElection always returns false, ensuring that the runtime configuration
// watcher is always invoked even if we don't hold the lock. This ensures we use the
// correct CA for renewing the serving cert, and that we're using the most up-to-date
// issuerRef for when we do acquire the lock.
func (rcw *RuntimeConfigurationWatcher) NeedLeaderElection() bool {
	return false
}

func (rcw *RuntimeConfigurationWatcher) Start(ctx context.Context) error {
	logger := rcw.m.log.WithName("runtime-config-watcher").WithValues("config-map-name", rcw.m.opts.IssuanceConfigMapName, "config-map-namespace", rcw.m.opts.IssuanceConfigMapNamespace)

LOOP:
	for {
		logger.Info("Starting / restarting watcher for runtime configuration")
		cmList := &corev1.ConfigMapList{}

		// First create a watcher. This is in a labelled loop in case the watcher dies for some reason
		// while we're running - in that case, we don't want to give up entirely on watching for runtime config
		// but instead we want to recreate the watcher.

		watcher, err := rcw.m.kubernetesClient.Watch(ctx, cmList, &client.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", rcw.m.opts.IssuanceConfigMapName),
			Namespace:     rcw.m.opts.IssuanceConfigMapNamespace,
		})

		if err != nil {
			logger.Error(err, "Failed to create ConfigMap watcher; will retry in 5s")
			time.Sleep(5 * time.Second)
			continue
		}

		for {
			// Now loop indefinitely until the main context cancels or we get an event to process.
			// If the main context cancels, we break out of the outer loop and this function returns.
			// If we get an event, we first check whether the channel closed. If so, we recreate the watcher by continuing
			// the outer loop.
			select {
			case <-ctx.Done():
				logger.Info("Received context cancellation, shutting down runtime configuration watcher")
				watcher.Stop()
				break LOOP

			case event, open := <-watcher.ResultChan():
				if !open {
					logger.Info("Received closed channel from ConfigMap watcher, will recreate")
					watcher.Stop()
					continue LOOP
				}

				switch event.Type {
				case watch.Deleted:
					rcw.m.handleRuntimeConfigIssuerDeletion(logger)

				case watch.Added:
					err := rcw.m.handleRuntimeConfigIssuerChange(logger, event)
					if err != nil {
						logger.Error(err, "Failed to handle new runtime configuration for issuerRef")
					}

				case watch.Modified:
					err := rcw.m.handleRuntimeConfigIssuerChange(logger, event)
					if err != nil {
						logger.Error(err, "Failed to handle runtime configuration issuerRef change")
					}

				case watch.Bookmark:
					// Ignore

				case watch.Error:
					err, ok := event.Object.(error)
					if !ok {
						logger.Error(nil, "Got an error event when watching runtime configuration but unable to determine further information")
					} else {
						logger.Error(err, "Got an error event when watching runtime configuration")
					}

				default:
					logger.Info("Got unknown event for runtime configuration ConfigMap; ignoring", "event-type", string(event.Type))
				}
			}
		}
	}

	logger.Info("Stopped runtime configuration watcher")
	return nil
}

func (m *manager) RuntimeConfigurationWatcher(ctx context.Context) ctrlmgr.Runnable {
	return &RuntimeConfigurationWatcher{
		m: m,
	}
}

var errNoOriginalIssuer = fmt.Errorf("no original issuer was provided")

func handleOriginalIssuerRef(in cmmeta.ObjectReference) (*cmmeta.ObjectReference, error) {
	if in.Name == "" && in.Kind == "" && in.Group == "" {
		return nil, errNoOriginalIssuer
	}

	if in.Name == "" {
		return nil, fmt.Errorf("issuerRef.Name is a required field if any field is set for issuerRef")
	}

	if in.Kind == "" {
		return nil, fmt.Errorf("issuerRef.Kind is a required field if any field is set for issuerRef")
	}

	if in.Group == "" {
		return nil, fmt.Errorf("issuerRef.Group is a required field if any field is set for issuerRef")
	}

	return &in, nil
}
