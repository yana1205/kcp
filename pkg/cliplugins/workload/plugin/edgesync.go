/*
Copyright 2022 The KCP Authors.

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

package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/google/uuid"
	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/martinlindhe/base36"
	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apix "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog/v2"
	"k8s.io/kube-openapi/pkg/util/sets"

	kcpclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	"github.com/kcp-dev/kcp/pkg/cliplugins/base"
	"github.com/kcp-dev/kcp/pkg/cliplugins/helpers"
	kcpfeatures "github.com/kcp-dev/kcp/pkg/features"
)

// EdgeSyncOptions contains options for configuring a SyncTarget and its corresponding syncer.
type EdgeSyncOptions struct {
	*base.Options

	// ResourcesToSync is a list of fully-qualified resource names that should be synced by the syncer.
	ResourcesToSync []string
	// APIExports is a list of APIExport to be supported by the synctarget.
	APIExports []string
	// SyncerImage is the container image that should be used for the syncer.
	SyncerImage string
	// Replicas is the number of replicas to configure in the syncer's deployment.
	Replicas int
	// OutputFile is the path to a file where the YAML for the syncer should be written.
	OutputFile string
	// DownstreamNamespace is the name of the namespace in the physical cluster where the syncer deployment is created.
	DownstreamNamespace string
	// KCPNamespace is the name of the namespace in the kcp workspace where the service account is created for the
	// syncer.
	KCPNamespace string
	// QPS is the refill rate for the syncer client's rate limiter bucket (steady state requests per second).
	QPS float32
	// Burst is the maximum size for the syncer client's rate limiter bucket when idle.
	Burst int
	// SyncTargetName is the name of the SyncTarget in the kcp workspace.
	SyncTargetName string
	// SyncTargetLabels are the labels to be applied to the SyncTarget in the kcp workspace.
	SyncTargetLabels []string
	// APIImportPollInterval is the time interval to push apiimport.
	APIImportPollInterval time.Duration
	// FeatureGates is used to configure which feature gates are enabled.
	FeatureGates string
	// DownstreamNamespaceCleanDelay is the time to wait before deleting of a downstream namespace.
	DownstreamNamespaceCleanDelay time.Duration
}

// NewSyncOptions returns a new EdgeSyncOptions.
func NewEdgeSyncOptions(streams genericclioptions.IOStreams) *EdgeSyncOptions {
	return &EdgeSyncOptions{
		Options: base.NewOptions(streams),

		Replicas:                      1,
		KCPNamespace:                  "default",
		QPS:                           20,
		Burst:                         30,
		APIImportPollInterval:         1 * time.Minute,
		APIExports:                    []string{"root:compute:kubernetes"},
		DownstreamNamespaceCleanDelay: 30 * time.Second,
	}
}

// BindFlags binds fields EdgeSyncOptions as command line flags to cmd's flagset.
func (o *EdgeSyncOptions) BindFlags(cmd *cobra.Command) {
	o.Options.BindFlags(cmd)

	cmd.Flags().StringSliceVar(&o.ResourcesToSync, "resources", o.ResourcesToSync, "Resources to synchronize with kcp, each resource should be in the format of resourcename.<gvr_of_the_resource>,"+
		"e.g. to sync routes to physical cluster the resource name should be given as --resource routes.route.openshift.io")
	cmd.Flags().StringSliceVar(&o.APIExports, "apiexports", o.APIExports,
		"APIExport to be supported by the syncer, each APIExport should be in the format of <absolute_ref_to_workspace>:<apiexport>, "+
			"e.g. root:compute:kubernetes is the kubernetes APIExport in root:compute workspace")
	cmd.Flags().StringVar(&o.SyncerImage, "syncer-image", o.SyncerImage, "The syncer image to use in the syncer's deployment YAML. Images are published at https://github.com/kcp-dev/kcp/pkgs/container/kcp%2Fsyncer.")
	cmd.Flags().IntVar(&o.Replicas, "replicas", o.Replicas, "Number of replicas of the syncer deployment.")
	cmd.Flags().StringVar(&o.KCPNamespace, "kcp-namespace", o.KCPNamespace, "The name of the kcp namespace to create a service account in.")
	cmd.Flags().StringVarP(&o.OutputFile, "output-file", "o", o.OutputFile, "The manifest file to be created and applied to the physical cluster. Use - for stdout.")
	cmd.Flags().StringVarP(&o.DownstreamNamespace, "namespace", "n", o.DownstreamNamespace, "The namespace to create the syncer in the physical cluster. By default this is \"kcp-syncer-<synctarget-name>-<uid>\".")
	cmd.Flags().Float32Var(&o.QPS, "qps", o.QPS, "QPS to use when talking to API servers.")
	cmd.Flags().IntVar(&o.Burst, "burst", o.Burst, "Burst to use when talking to API servers.")
	cmd.Flags().StringVar(&o.FeatureGates, "feature-gates", o.FeatureGates,
		"A set of key=value pairs that describe feature gates for alpha/experimental features. "+
			"Options are:\n"+strings.Join(kcpfeatures.KnownFeatures(), "\n")) // hide kube-only gates
	cmd.Flags().DurationVar(&o.APIImportPollInterval, "api-import-poll-interval", o.APIImportPollInterval, "Polling interval for API import.")
	cmd.Flags().DurationVar(&o.DownstreamNamespaceCleanDelay, "downstream-namespace-clean-delay", o.DownstreamNamespaceCleanDelay, "Time to wait before deleting a downstream namespaces.")
	cmd.Flags().StringSliceVar(&o.SyncTargetLabels, "labels", o.SyncTargetLabels, "Labels to apply on the SyncTarget created in kcp, each label should be in the format of key=value.")
}

// Complete ensures all dynamically populated fields are initialized.
func (o *EdgeSyncOptions) Complete(args []string) error {
	if err := o.Options.Complete(); err != nil {
		return err
	}

	o.SyncTargetName = args[0]

	return nil
}

// Validate validates the EdgeSyncOptions are complete and usable.
func (o *EdgeSyncOptions) Validate() error {
	var errs []error

	if err := o.Options.Validate(); err != nil {
		errs = append(errs, err)
	}

	if o.SyncerImage == "" {
		errs = append(errs, errors.New("--syncer-image is required"))
	}

	if o.KCPNamespace == "" {
		errs = append(errs, errors.New("--kcp-namespace is required"))
	}

	if o.Replicas < 0 {
		errs = append(errs, errors.New("--replicas cannot be negative"))
	}
	if o.Replicas > 1 {
		// TODO: relax when we have leader-election in the syncer
		errs = append(errs, errors.New("only 0 and 1 are valid values for --replicas"))
	}

	if o.OutputFile == "" {
		errs = append(errs, errors.New("--output-file is required"))
	}

	// see pkg/syncer/shared/GetDNSID
	if len(o.SyncTargetName)+len(DNSIDPrefix)+8+8+2 > 254 {
		errs = append(errs, fmt.Errorf("the maximum length of the sync-target-name is %d", MaxSyncTargetNameLength))
	}

	for _, l := range o.SyncTargetLabels {
		if len(strings.Split(l, "=")) != 2 {
			errs = append(errs, fmt.Errorf("label '%s' is not in the format of key=value", l))
		}
	}

	return utilerrors.NewAggregate(errs)
}

// Run prepares a kcp workspace for use with a syncer and outputs the
// configuration required to deploy a syncer to the pcluster to stdout.
func (o *EdgeSyncOptions) Run(ctx context.Context) error {
	config, err := o.ClientConfig.ClientConfig()
	if err != nil {
		return err
	}

	var outputFile *os.File
	if o.OutputFile == "-" {
		outputFile = os.Stdout
	} else {
		outputFile, err = os.Create(o.OutputFile)
		if err != nil {
			return err
		}
		defer outputFile.Close()
	}

	labels := map[string]string{}
	for _, l := range o.SyncTargetLabels {
		parts := strings.Split(l, "=")
		if len(parts) != 2 {
			continue
		}
		labels[parts[0]] = parts[1]
	}

	token, syncerID, syncTarget, err := o.enableSyncerForWorkspace(ctx, config, o.SyncTargetName, o.KCPNamespace, labels)
	if err != nil {
		return err
	}

	// TODO: For now, set nothing but in future, need the list of resources to be synced/upsynced. For EMC Syncer, we assume there come from Edge Placement
	expectedResourcesForPermission := sets.NewString()

	configURL, _, err := helpers.ParseClusterURL(config.Host)
	if err != nil {
		return fmt.Errorf("current URL %q does not point to workspace", config.Host)
	}

	// Make sure the generated URL has the port specified correctly.
	if _, _, err = net.SplitHostPort(configURL.Host); err != nil {
		var addrErr *net.AddrError
		const missingPort = "missing port in address"
		if errors.As(err, &addrErr) && addrErr.Err == missingPort {
			if configURL.Scheme == "https" {
				configURL.Host = net.JoinHostPort(configURL.Host, "443")
			} else {
				configURL.Host = net.JoinHostPort(configURL.Host, "80")
			}
		} else {
			return fmt.Errorf("failed to parse host %q: %w", configURL.Host, err)
		}
	}

	if o.DownstreamNamespace == "" {
		o.DownstreamNamespace = syncerID
	}

	// Compose the syncer's upstream configuration server URL without any path. This is
	// required so long as the API importer and syncer expect to require cluster clients.
	//
	// TODO(marun) It's probably preferable that the syncer and importer are provided a
	// cluster configuration since they only operate against a single workspace.
	serverURL := configURL.Scheme + "://" + configURL.Host
	input := templateInputForEdge{
		ServerURL:    serverURL,
		CAData:       base64.StdEncoding.EncodeToString(config.CAData),
		Token:        token,
		KCPNamespace: o.KCPNamespace,
		Namespace:    o.DownstreamNamespace,

		SyncTargetPath: logicalcluster.From(syncTarget).Path().String(),
		SyncTarget:     o.SyncTargetName,
		SyncTargetUID:  string(syncTarget.UID),

		Image:                               o.SyncerImage,
		Replicas:                            o.Replicas,
		ResourcesToSync:                     o.ResourcesToSync,
		QPS:                                 o.QPS,
		Burst:                               o.Burst,
		FeatureGatesString:                  o.FeatureGates,
		APIImportPollIntervalString:         o.APIImportPollInterval.String(),
		DownstreamNamespaceCleanDelayString: o.DownstreamNamespaceCleanDelay.String(),
	}

	resources, err := renderEdgeSyncerResources(input, syncerID, expectedResourcesForPermission.List())
	if err != nil {
		return err
	}

	_, err = outputFile.Write(resources)
	if o.OutputFile != "-" {
		fmt.Fprintf(o.ErrOut, "\nWrote physical cluster manifest to %s for namespace %q. Use\n\n  KUBECONFIG=<pcluster-config> kubectl apply -f %q\n\nto apply it. "+
			"Use\n\n  KUBECONFIG=<pcluster-config> kubectl get deployment -n %q %s\n\nto verify the syncer pod is running.\n", o.OutputFile, o.DownstreamNamespace, o.OutputFile, o.DownstreamNamespace, syncerID)
	}
	return err
}

// getEdgeSyncerID returns a unique ID for a syncer derived from the name and its UID. It's
// a valid DNS segment and can be used as namespace or object names.
func getEdgeSyncerID(syncTarget *typeEdgeSyncTarget) string {
	syncerHash := sha256.Sum224([]byte(syncTarget.UID))
	base36hash := strings.ToLower(base36.EncodeBytes(syncerHash[:]))
	return fmt.Sprintf("kcp-edge-syncer-%s-%s", syncTarget.Name, base36hash[:8])
}

type typeEdgeSyncTarget struct {
	UID         types.UID
	Name        string
	Annotations map[string]string
}

func (o *typeEdgeSyncTarget) GetAnnotations() map[string]string {
	return o.Annotations
}

func (o *EdgeSyncOptions) applySyncTarget(ctx context.Context, kcpClient kcpclient.Interface, syncTargetName string, labels map[string]string) (*typeEdgeSyncTarget, error) {
	logicalCluster, err := kcpClient.CoreV1alpha1().LogicalClusters().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get default logical cluster %q: %w", syncTargetName, err)
	}
	uuid, err := uuid.NewUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to generate UUID %q: %w", syncTargetName, err)
	}
	edgeSyncTarget := typeEdgeSyncTarget{
		UID:         types.UID(uuid.String()),
		Name:        syncTargetName,
		Annotations: logicalCluster.Annotations,
	}
	return &edgeSyncTarget, nil
}

// getResourcesForPermission get all resources to sync from syncTarget status and resources flags. It is used to generate the rbac on
// physical cluster for syncer.
func (o *EdgeSyncOptions) getResourcesForPermission(ctx context.Context, config *rest.Config, syncTargetName string) (sets.String, error) {
	kcpClient, err := kcpclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kcp client: %w", err)
	}

	// Poll synctarget to get all resources to sync, the ResourcesToSync set from the flag should be also added, since
	// its related APIResourceSchemas will not be added until the syncer is started.
	expectedResourcesForPermission := sets.NewString(o.ResourcesToSync...)
	// secrets and configmaps are always needed.
	expectedResourcesForPermission.Insert("secrets", "configmaps")
	err = wait.PollImmediateWithContext(ctx, 100*time.Millisecond, 30*time.Second, func(ctx context.Context) (bool, error) {
		syncTarget, err := kcpClient.WorkloadV1alpha1().SyncTargets().Get(ctx, syncTargetName, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr
		}

		// skip if there is only the local kubernetes APIExport in the synctarget workspace, since we may not get syncedResources yet.
		clusterName := logicalcluster.From(syncTarget)

		if len(syncTarget.Spec.SupportedAPIExports) == 1 &&
			syncTarget.Spec.SupportedAPIExports[0].Export == "kubernetes" &&
			(len(syncTarget.Spec.SupportedAPIExports[0].Path) == 0 ||
				syncTarget.Spec.SupportedAPIExports[0].Path == clusterName.String()) {
			return true, nil
		}

		if len(syncTarget.Status.SyncedResources) == 0 {
			return false, nil
		}
		for _, rs := range syncTarget.Status.SyncedResources {
			expectedResourcesForPermission.Insert(fmt.Sprintf("%s.%s", rs.Resource, rs.Group))
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error waiting for getting resources to sync in syncTarget %s, %w", syncTargetName, err)
	}

	return expectedResourcesForPermission, nil
}

// enableSyncerForWorkspace creates a sync target with the given name and creates a service
// account for the syncer in the given namespace. The expectation is that the provided config is
// for a logical cluster (workspace). Returns the token the syncer will use to connect to kcp.
func (o *EdgeSyncOptions) enableSyncerForWorkspace(ctx context.Context, config *rest.Config, syncTargetName, namespace string, labels map[string]string) (saToken string, syncerID string, syncTarget *typeEdgeSyncTarget, err error) {
	kcpClient, err := kcpclient.NewForConfig(config)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create kcp client: %w", err)
	}

	syncTarget, err = o.applySyncTarget(ctx, kcpClient, syncTargetName, labels)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to apply synctarget %q: %w", syncTargetName, err)
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	apixClientSet, err := apix.NewForConfig(config)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to create apiextension kubernetes client: %w", err)
	}
	crdByte, err := embeddedResources.ReadFile("edge.kcp.io_edgesyncconfigs.yaml")
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to load edgeSyncConfig CRD yaml: %w", err)
	}
	decoder := yaml.NewYAMLToJSONDecoder(bytes.NewReader(crdByte))

	var u unstructured.Unstructured
	err = decoder.Decode(&u)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to decode edgeSyncConfig CRD yaml: %w", err)
	}
	var crd apixv1.CustomResourceDefinition
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &crd)
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to convert to edgeSyncConfig CRD: %w", err)
	}

	_, err = apixClientSet.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = apixClientSet.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{})
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to create edgeSyncConfig CRD: %w", err)
		}
	}

	var syncConfig *unstructured.Unstructured
	if err := wait.PollImmediateInfiniteWithContext(ctx, time.Second*1, func(ctx context.Context) (bool, error) {
		syncConfig, err = createEdgeSyncConfig(ctx, config, syncTargetName)
		if err != nil {
			_ = err
			return false, nil
		}
		return true, nil
	}); err != nil {
		return "", "", nil, fmt.Errorf("failed to get or create EdgeSyncConfig resource: %w", err)
	}
	syncerID = getEdgeSyncerID(syncTarget)
	syncTargetOwnerReferences := []metav1.OwnerReference{{
		APIVersion: syncConfig.GetAPIVersion(),
		Kind:       syncConfig.GetKind(),
		Name:       syncConfig.GetName(),
		UID:        syncConfig.GetUID(),
	}}
	sa, err := kubeClient.CoreV1().ServiceAccounts(namespace).Get(ctx, syncerID, metav1.GetOptions{})

	switch {
	case apierrors.IsNotFound(err):
		fmt.Fprintf(o.ErrOut, "Creating service account %q\n", syncerID)
		if sa, err = kubeClient.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:            syncerID,
				OwnerReferences: syncTargetOwnerReferences,
			},
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", "", nil, fmt.Errorf("failed to create ServiceAccount %s|%s/%s: %w", syncTargetName, namespace, syncerID, err)
		}
	case err == nil:
		oldData, err := json.Marshal(corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: sa.OwnerReferences,
			},
		})
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to marshal old data for ServiceAccount %s|%s/%s: %w", syncTargetName, namespace, syncerID, err)
		}

		newData, err := json.Marshal(corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				UID:             sa.UID,
				ResourceVersion: sa.ResourceVersion,
				OwnerReferences: mergeOwnerReferenceForEdge(sa.ObjectMeta.OwnerReferences, syncTargetOwnerReferences),
			},
		})
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to marshal new data for ServiceAccount %s|%s/%s: %w", syncTargetName, namespace, syncerID, err)
		}

		patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to create patch for ServiceAccount %s|%s/%s: %w", syncTargetName, namespace, syncerID, err)
		}

		fmt.Fprintf(o.ErrOut, "Updating service account %q.\n", syncerID)
		if sa, err = kubeClient.CoreV1().ServiceAccounts(namespace).Patch(ctx, sa.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			return "", "", nil, fmt.Errorf("failed to patch ServiceAccount %s|%s/%s: %w", syncTargetName, syncerID, namespace, err)
		}
	default:
		return "", "", nil, fmt.Errorf("failed to get the ServiceAccount %s|%s/%s: %w", syncTargetName, syncerID, namespace, err)
	}

	// Create a cluster role that provides the syncer the minimal permissions
	// required by KCP to manage the sync target, and by the syncer virtual
	// workspace to sync.
	rules := []rbacv1.PolicyRule{
		{
			Verbs:     []string{"*"},
			APIGroups: []string{"*"},
			Resources: []string{"*"},
		},
		{
			Verbs:           []string{"access"},
			NonResourceURLs: []string{"/"},
		},
	}

	cr, err := kubeClient.RbacV1().ClusterRoles().Get(ctx,
		syncerID,
		metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		fmt.Fprintf(o.ErrOut, "Creating cluster role %q to give service account %q\n\n 1. write and sync access to the synctarget %q\n 2. write access to apiresourceimports.\n\n", syncerID, syncerID, syncerID)
		if _, err = kubeClient.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:            syncerID,
				OwnerReferences: syncTargetOwnerReferences,
			},
			Rules: rules,
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", "", nil, err
		}
	case err == nil:
		oldData, err := json.Marshal(rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: cr.OwnerReferences,
			},
			Rules: cr.Rules,
		})
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to marshal old data for ClusterRole %s|%s: %w", syncTargetName, syncerID, err)
		}

		newData, err := json.Marshal(rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				UID:             cr.UID,
				ResourceVersion: cr.ResourceVersion,
				OwnerReferences: mergeOwnerReferenceForEdge(cr.OwnerReferences, syncTargetOwnerReferences),
			},
			Rules: rules,
		})
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to marshal new data for ClusterRole %s|%s: %w", syncTargetName, syncerID, err)
		}

		patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
		if err != nil {
			return "", "", nil, fmt.Errorf("failed to create patch for ClusterRole %s|%s: %w", syncTargetName, syncerID, err)
		}

		fmt.Fprintf(o.ErrOut, "Updating cluster role %q with\n\n 1. write and sync access to the synctarget %q\n 2. write access to apiresourceimports.\n\n", syncerID, syncerID)
		if _, err = kubeClient.RbacV1().ClusterRoles().Patch(ctx, cr.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{}); err != nil {
			return "", "", nil, fmt.Errorf("failed to patch ClusterRole %s|%s/%s: %w", syncTargetName, syncerID, namespace, err)
		}
	default:
		return "", "", nil, err
	}

	// Grant the service account the role created just above in the workspace
	subjects := []rbacv1.Subject{{
		Kind:      "ServiceAccount",
		Name:      syncerID,
		Namespace: namespace,
	}}
	roleRef := rbacv1.RoleRef{
		Kind:     "ClusterRole",
		Name:     syncerID,
		APIGroup: "rbac.authorization.k8s.io",
	}

	_, err = kubeClient.RbacV1().ClusterRoleBindings().Get(ctx,
		syncerID,
		metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", "", nil, err
	}
	if err == nil {
		if err := kubeClient.RbacV1().ClusterRoleBindings().Delete(ctx, syncerID, metav1.DeleteOptions{}); err != nil {
			return "", "", nil, err
		}
	}

	fmt.Fprintf(o.ErrOut, "Creating or updating cluster role binding %q to bind service account %q to cluster role %q.\n", syncerID, syncerID, syncerID)
	if _, err = kubeClient.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            syncerID,
			OwnerReferences: syncTargetOwnerReferences,
		},
		Subjects: subjects,
		RoleRef:  roleRef,
	}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", "", nil, err
	}

	// Wait for the service account to be updated with the name of the token secret
	tokenSecretName := ""
	err = wait.PollImmediateWithContext(ctx, 100*time.Millisecond, 20*time.Second, func(ctx context.Context) (bool, error) {
		serviceAccount, err := kubeClient.CoreV1().ServiceAccounts(namespace).Get(ctx, sa.Name, metav1.GetOptions{})
		if err != nil {
			klog.FromContext(ctx).V(5).WithValues("err", err).Info("failed to retrieve ServiceAccount")
			return false, nil
		}
		if len(serviceAccount.Secrets) == 0 {
			return false, nil
		}
		tokenSecretName = serviceAccount.Secrets[0].Name
		return true, nil
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("timed out waiting for token secret name to be set on ServiceAccount %s/%s", namespace, sa.Name)
	}

	// Retrieve the token that the syncer will use to authenticate to kcp
	tokenSecret, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, tokenSecretName, metav1.GetOptions{})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to retrieve Secret: %w", err)
	}
	saTokenBytes := tokenSecret.Data["token"]
	if len(saTokenBytes) == 0 {
		return "", "", nil, fmt.Errorf("token secret %s/%s is missing a value for `token`", namespace, tokenSecretName)
	}

	return string(saTokenBytes), syncerID, syncTarget, nil
}

// mergeOwnerReferenceForEdge: merge a slice of ownerReference with a given ownerReferences.
func mergeOwnerReferenceForEdge(ownerReferences, newOwnerReferences []metav1.OwnerReference) []metav1.OwnerReference {
	var merged []metav1.OwnerReference

	merged = append(merged, ownerReferences...)

	for _, ownerReference := range newOwnerReferences {
		found := false
		for _, mergedOwnerReference := range merged {
			if mergedOwnerReference.UID == ownerReference.UID {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, ownerReference)
		}
	}

	return merged
}

// templateInputForEdge represents the external input required to render the resources to
// deploy the syncer to a pcluster.
type templateInputForEdge struct {
	// ServerURL is the logical cluster url the syncer configuration will use
	ServerURL string
	// CAData holds the PEM-encoded bytes of the ca certificate(s) a syncer will use to validate
	// kcp's serving certificate
	CAData string
	// Token is the service account token used to authenticate a syncer for access to a workspace
	Token string
	// KCPNamespace is the name of the kcp namespace of the syncer's service account
	KCPNamespace string
	// Namespace is the name of the syncer namespace on the pcluster
	Namespace string
	// SyncTargetPath is the qualified kcp logical cluster name the syncer will sync from
	SyncTargetPath string
	// SyncTarget is the name of the sync target the syncer will use to
	// communicate its status and read configuration from
	SyncTarget string
	// SyncTargetUID is the UID of the sync target the syncer will use to
	// communicate its status and read configuration from. This information is used by the
	// Syncer in order to avoid a conflict when a synctarget gets deleted and another one is
	// created with the same name.
	SyncTargetUID string
	// ResourcesToSync is the set of qualified resource names (eg. ["services",
	// "deployments.apps.k8s.io") that the syncer will synchronize between the kcp
	// workspace and the pcluster.
	ResourcesToSync []string
	// Image is the name of the container image that the syncer deployment will use
	Image string
	// Replicas is the number of syncer pods to run (should be 0 or 1).
	Replicas int
	// QPS is the qps the syncer uses when talking to an apiserver.
	QPS float32
	// Burst is the burst the syncer uses when talking to an apiserver.
	Burst int
	// FeatureGatesString is the set of features gates.
	FeatureGatesString string
	// APIImportPollIntervalString is the string of interval to poll APIImport.
	APIImportPollIntervalString string
	// DownstreamNamespaceCleanDelay is the time to delay before cleaning the downstream namespace as a string.
	DownstreamNamespaceCleanDelayString string
}

// templateArgsForEdge represents the full set of arguments required to render the resources
// required to deploy the syncer.
type templateArgsForEdge struct {
	templateInputForEdge
	// ServiceAccount is the name of the service account to create in the syncer
	// namespace on the pcluster.
	ServiceAccount string
	// ClusterRole is the name of the cluster role to create for the syncer on the
	// pcluster.
	ClusterRole string
	// ClusterRoleBinding is the name of the cluster role binding to create for the
	// syncer on the pcluster.
	ClusterRoleBinding string
	// DnsRole is the name of the DNS role to create for the syncer on the pcluster.
	DNSRole string
	// DNSRoleBinding is the name of the DNS role binding to create for the
	// syncer on the pcluster.
	DNSRoleBinding string
	// GroupMappings is the mapping of api group to resources that will be used to
	// define the cluster role rules for the syncer in the pcluster. The syncer will be
	// granted full permissions for the resources it will synchronize.
	GroupMappings []groupMappingForEdge
	// Secret is the name of the secret that will contain the kubeconfig the syncer
	// will use to connect to the kcp logical cluster (workspace) that it will
	// synchronize from.
	Secret string
	// Key in the syncer secret for the kcp logical cluster kubconfig.
	SecretConfigKey string
	// Deployment is the name of the deployment that will run the syncer in the
	// pcluster.
	Deployment string
	// DeploymentApp is the label value that the syncer's deployment will select its
	// pods with.
	DeploymentApp string
}

// renderEdgeSyncerResources renders the resources required to deploy a syncer to a pcluster.
//
// TODO(marun) Is it possible to set owner references in a set of applied resources? Ideally the
// cluster role and role binding would be owned by the namespace to ensure cleanup on deletion
// of the namespace.
func renderEdgeSyncerResources(input templateInputForEdge, syncerID string, resourceForPermission []string) ([]byte, error) {
	dnsSyncerID := strings.Replace(syncerID, "syncer", "dns", 1)

	tmplArgs := templateArgsForEdge{
		templateInputForEdge: input,
		ServiceAccount:       syncerID,
		ClusterRole:          syncerID,
		ClusterRoleBinding:   syncerID,
		DNSRole:              dnsSyncerID,
		DNSRoleBinding:       dnsSyncerID,
		GroupMappings:        getGroupMappingsForEdge(resourceForPermission),
		Secret:               syncerID,
		SecretConfigKey:      SyncerSecretConfigKey,
		Deployment:           syncerID,
		DeploymentApp:        syncerID,
	}

	syncerTemplate, err := embeddedResources.ReadFile("edge-syncer.yaml")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("syncerTemplate").Parse(string(syncerTemplate))
	if err != nil {
		return nil, err
	}
	buffer := bytes.NewBuffer([]byte{})
	err = tmpl.Execute(buffer, tmplArgs)
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// groupMappingForEdge associates an api group to the resources in that group.
type groupMappingForEdge struct {
	APIGroup  string
	Resources []string
}

// getGroupMappingsForEdge returns the set of api groups to resources for the given resources.
func getGroupMappingsForEdge(resourcesToSync []string) []groupMappingForEdge {
	groupMap := make(map[string][]string)

	for _, resource := range resourcesToSync {
		nameParts := strings.SplitN(resource, ".", 2)
		name := nameParts[0]
		apiGroup := ""
		if len(nameParts) > 1 {
			apiGroup = nameParts[1]
		}
		if _, ok := groupMap[apiGroup]; !ok {
			groupMap[apiGroup] = []string{name}
		} else {
			groupMap[apiGroup] = append(groupMap[apiGroup], name)
		}
		// If pods are being synced, add the subresources that are required to
		// support the pod subresources.
		if apiGroup == "" && name == "pods" {
			podSubresources := []string{
				"pods/log",
				"pods/exec",
				"pods/attach",
				"pods/binding",
				"pods/portforward",
				"pods/proxy",
				"pods/ephemeralcontainers",
			}
			groupMap[apiGroup] = append(groupMap[apiGroup], podSubresources...)
		}
	}

	groupMappings := make([]groupMappingForEdge, 0, len(groupMap))
	for apiGroup, resources := range groupMap {
		groupMappings = append(groupMappings, groupMappingForEdge{
			APIGroup:  apiGroup,
			Resources: resources,
		})
	}

	sortGroupMappingsForEdge(groupMappings)

	return groupMappings
}

// sortGroupMappingsForEdge sorts group mappings first by APIGroup and then by Resources.
func sortGroupMappingsForEdge(groupMappings []groupMappingForEdge) {
	sort.Slice(groupMappings, func(i, j int) bool {
		if groupMappings[i].APIGroup == groupMappings[j].APIGroup {
			return strings.Join(groupMappings[i].Resources, ",") < strings.Join(groupMappings[j].Resources, ",")
		}
		return groupMappings[i].APIGroup < groupMappings[j].APIGroup
	})
}

func createEdgeSyncConfig(ctx context.Context, cfg *rest.Config, syncTargetName string) (*unstructured.Unstructured, error) {
	dyClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client :%w", err)
	}
	discoveryClient := discovery.NewDiscoveryClientForConfigOrDie(cfg)
	gk := schema.GroupKind{
		Group: "edge.kcp.io",
		Kind:  "EdgeSyncConfig",
	}
	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get API group resources :%w", err)
	}
	restMapper := restmapper.NewDiscoveryRESTMapper(groupResources)
	mapping, err := restMapper.RESTMapping(gk, "v1alpha1")
	if err != nil || mapping == nil {
		return nil, fmt.Errorf("failed to get resource mapping :%w", err)
	}
	cr, err := dyClient.Resource(mapping.Resource).Get(ctx, syncTargetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cr = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": gk.Group + "/v1alpha1",
			"kind":       gk.Kind,
			"metadata": map[string]interface{}{
				"name": syncTargetName,
			},
			"spec": map[string]interface{}{},
		}}
		cr, err := dyClient.Resource(mapping.Resource).Create(ctx, cr, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create EdgeSyncConfig :%w", err)
		}
		return cr, nil
	} else {
		return cr, nil
	}
}
