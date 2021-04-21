// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package sync

import (
	"context"
	"fmt"
	"strings"

	clusterv1 "github.com/open-cluster-management/api/cluster/v1"
	appsv1 "github.com/open-cluster-management/governance-policy-propagator/pkg/apis/apps/v1"
	policiesv1 "github.com/open-cluster-management/governance-policy-propagator/pkg/apis/policy/v1"
	"github.com/open-cluster-management/governance-policy-propagator/pkg/controller/common"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const controllerName string = "policy-spec-sync"

var log = logf.Log.WithName(controllerName)

// Add creates a new Policy Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, managedCfg *rest.Config) error {
	managedClient, err := client.New(managedCfg, client.Options{})
	if err != nil {
		log.Error(err, "Failed to generate client to managed cluster")
		return err
	}
	var kubeClient kubernetes.Interface = kubernetes.NewForConfigOrDie(managedCfg)
	eventsScheme := runtime.NewScheme()
	if err = v1.AddToScheme(eventsScheme); err != nil {
		return err
	}

	eventBroadcaster := record.NewBroadcaster()
	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Error(err, "Failed to get watch namespace")
		return err
	}
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events(namespace)})
	managedRecorder := eventBroadcaster.NewRecorder(eventsScheme, v1.EventSource{Component: controllerName})

	return add(mgr, newReconciler(mgr, managedClient, managedRecorder))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, managedClient client.Client,
	managedRecorder record.EventRecorder) reconcile.Reconciler {
	return &ReconcilePolicy{hubClient: mgr.GetClient(), managedClient: managedClient,
		managedRecorder: managedRecorder, scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Policy
	err = c.Watch(&source.Kind{Type: &policiesv1.Policy{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcilePolicy implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcilePolicy{}

// ReconcilePolicy reconciles a Policy object
type ReconcilePolicy struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	hubClient       client.Client
	managedClient   client.Client
	managedRecorder record.EventRecorder
	scheme          *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Policy object and makes changes based on the state read
// and what is in the Policy.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcilePolicy) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Policy...")

	// Fetch the Policy instance
	instance := &policiesv1.Policy{}
	err := r.hubClient.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// repliated policy on hub was deleted, remove policy on managed cluster
			reqLogger.Info("Policy was deleted, removing on managed cluster...")
			err = r.managedClient.Delete(context.TODO(), &policiesv1.Policy{
				TypeMeta: metav1.TypeMeta{
					Kind:       policiesv1.Kind,
					APIVersion: policiesv1.SchemeGroupVersion.Group,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      request.Name,
					Namespace: request.Namespace,
				},
			})
			if err != nil && !errors.IsNotFound(err) {
				reqLogger.Error(err, "Failed to remove policy on managed cluster...")
			}
			reqLogger.Info("Policy has been removed from managed cluster...Reconciliation complete.")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Failed to get policy from hub...")
		return reconcile.Result{}, err
	}

	plcToReplicate := instance
	namespacedNameOfPlcToReplicate := request.NamespacedName

	clusterName := instance.Labels[common.ClusterNameLabel]
	reqLogger.Info(fmt.Sprintf("the cluster of the policy is %s", clusterName))
	managedCluster := &clusterv1.ManagedCluster{}
	err = r.hubClient.Get(context.TODO(), types.NamespacedName{Name: clusterName}, managedCluster)
	if err != nil {
		reqLogger.Error(err, "Failed to get managed cluster...")
		return reconcile.Result{}, err
	}

	hubPolicyLabel := ""

	// TODO create a constant for hub.open-cluster-management.io
	if managedCluster.Labels["hub.open-cluster-management.io"] == "true" {
		reqLogger.Info("the managed cluster of the policy is a hub")
		rootPlcDotNotationName := instance.Labels[common.RootPolicyLabel]
		reqLogger.Info(fmt.Sprintf("the root policy is %s", rootPlcDotNotationName))

		rootPlcName := strings.Split(rootPlcDotNotationName, ".")[1]
		rootPlcNamespace := strings.Split(rootPlcDotNotationName, ".")[0]
		reqLogger.Info(fmt.Sprintf("the root policy name is %s in namespace %s", rootPlcName, rootPlcNamespace))

		namespacedNameOfPlcToReplicate = types.NamespacedName{Namespace: instance.Namespace, Name: rootPlcName}

		r.replicatePlacementRulesAndBindings(instance, instance.Namespace, rootPlcName)
		hubPolicyLabel = rootPlcDotNotationName
	}

	managedPlc := &policiesv1.Policy{}
	err = r.managedClient.Get(context.TODO(), namespacedNameOfPlcToReplicate, managedPlc)
	if err != nil {
		if errors.IsNotFound(err) {
			// not found on managed cluster, create it
			reqLogger.Info("Policy not found on managed cluster, creating it...")
			managedPlc = plcToReplicate.DeepCopy()
			managedPlc.SetOwnerReferences(nil)
			managedPlc.SetResourceVersion("")
			managedPlc.SetName(namespacedNameOfPlcToReplicate.Name)
			managedPlc.SetNamespace(namespacedNameOfPlcToReplicate.Namespace)

			//TODO add constant for "hub-policy-name.open-cluster-management.io"
			if hubPolicyLabel != "" {
				managedPlc.SetLabels(map[string]string{"hub-policy-name.open-cluster-management.io": hubPolicyLabel})
			}

			err = r.managedClient.Create(context.TODO(), managedPlc)
			if err != nil {
				reqLogger.Error(err, "Failed to create policy on managed...")
				return reconcile.Result{}, err
			}
			r.managedRecorder.Event(instance, "Normal", "PolicySpecSync",
				fmt.Sprintf("Policy %s was synchronized to cluster namespace %s", plcToReplicate.GetName(),
					plcToReplicate.GetNamespace()))
		} else {
			reqLogger.Error(err, "Failed to get policy from managed...")
			return reconcile.Result{}, err
		}
	}
	// found, then compare and update
	if !common.CompareSpecAndAnnotation(plcToReplicate, managedPlc) {
		// update needed
		reqLogger.Info("Policy mismatch between hub and managed, updating it...")
		managedPlc.SetAnnotations(plcToReplicate.GetAnnotations())
		managedPlc.Spec = plcToReplicate.Spec
		err = r.managedClient.Update(context.TODO(), managedPlc)
		if err != nil && errors.IsNotFound(err) {
			reqLogger.Error(err, "Failed to update policy on managed...")
			return reconcile.Result{}, err
		}
		r.managedRecorder.Event(instance, "Normal", "PolicySpecSync",
			fmt.Sprintf("Policy %s was updated in cluster namespace %s", plcToReplicate.GetName(),
				plcToReplicate.GetNamespace()))
	}
	reqLogger.Info("Reconciliation complete.")
	return reconcile.Result{}, nil
}

func (r *ReconcilePolicy) replicatePlacementRulesAndBindings(policy *policiesv1.Policy, rootNamespace, rootPolicyName string) {
     reqLogger := log.WithValues("Policy-Namespace", policy.GetNamespace(), "Policy-Name", policy.GetName())
     reqLogger.Info(fmt.Sprintf("Copy placement rules and bindings for policy %s into namespace %s", policy.GetName(), rootNamespace))

	// get binding
	pbList := &policiesv1.PlacementBindingList{}
	err := r.hubClient.List(context.TODO(), pbList, &client.ListOptions{Namespace: policy.GetNamespace()})
	if err != nil {
		reqLogger.Error(err, "Failed to list pb...")
		return
	}

	for _, pb := range pbList.Items {
		subjects := pb.Subjects
		for _, subject := range subjects {
			if subject.APIGroup == policiesv1.SchemeGroupVersion.Group &&
				subject.Kind == policiesv1.Kind && subject.Name == rootPolicyName {
				plr := &appsv1.PlacementRule{}
				err := r.hubClient.Get(context.TODO(), types.NamespacedName{Namespace: policy.GetNamespace(),
					Name: pb.PlacementRef.Name}, plr)
				if err != nil {
					reqLogger.Error(err, "Failed to get plr...", "Namespace", policy.GetNamespace(), "Name",
						pb.PlacementRef.Name)
					return
				}

				r.handlePlacementRule(plr, rootPolicyName, rootNamespace)
				r.handlePlacementBinding(&pb, rootPolicyName, rootNamespace)
			}
		}
	}
}

func (r *ReconcilePolicy) handlePlacementRule(plr *appsv1.PlacementRule, policyName, rootNamespace string) {
	reqLogger := log.WithValues("Policy-Namespace", rootNamespace, "Policy-Name", policyName)
	reqLogger.Info(fmt.Sprintf("Copy placement rule %s into namespace %s", plr.GetName(), rootNamespace))

	replicatedPlr := &appsv1.PlacementRule{}
	err := r.managedClient.Get(context.TODO(), types.NamespacedName{Namespace: rootNamespace,
					Name: plr.GetName()}, replicatedPlr)

	if err == nil {
		//TODO handle update of the replicated placement rule
		return
	}

	replicatedPlr = plr.DeepCopy()
	replicatedPlr.SetNamespace(rootNamespace)
	replicatedPlr.SetResourceVersion("")

	err = r.managedClient.Create(context.TODO(), replicatedPlr)
	if err != nil {
		// failed to create replicated object, requeue
		reqLogger.Error(err, "Failed to create replicated placement rule...", "Namespace", plr.GetNamespace(), "Name", plr.GetName())
	}
}

func (r *ReconcilePolicy) handlePlacementBinding(pb *policiesv1.PlacementBinding, policyName, rootNamespace string) {
	reqLogger := log.WithValues("Policy-Namespace", rootNamespace, "Policy-Name", policyName)
	reqLogger.Info(fmt.Sprintf("Copy placement binding %s into namespace %s", pb.GetName(), rootNamespace))

	replicatedPb := &policiesv1.PlacementBinding{}
	err := r.managedClient.Get(context.TODO(), types.NamespacedName{Namespace: rootNamespace,
					Name: pb.GetName()}, replicatedPb)

	if err == nil {
		//TODO handle update of the replicated placement binding
		return
	}

	replicatedPb = pb.DeepCopy()
	replicatedPb.SetNamespace(rootNamespace)
	replicatedPb.SetResourceVersion("")

	err = r.managedClient.Create(context.TODO(), replicatedPb)
	if err != nil {
		// failed to create replicated object, requeue
		reqLogger.Error(err, "Failed to create replicated placement binding...", "Namespace", pb.GetNamespace(), "Name", pb.GetName())
	}
}
