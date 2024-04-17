/*
Copyright 2024.

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

package controller

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	controlplanev1 "github.com/openshift-assisted/agent-controlplane-provider/api/v1"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	agentControlPlaneKind       = "AgentControlPlane"
	agentControlPlaneAnnotation = "controlplane.openshift.io/agentControlPlane"
)

// AgentControlPlaneReconciler reconciles a AgentControlPlane object
type AgentControlPlaneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=controlplane.openshift.io,resources=agentcontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.openshift.io,resources=agentcontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=controlplane.openshift.io,resources=agentcontrolplanes/finalizers,verbs=update
//+kubebuilder:rbac:groups=agent-install.openshift.io,resources=infraenvs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agent-install.openshift.io,resources=infraenvs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AgentControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Get AgentControlPlane instance
	acp := &controlplanev1.AgentControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, acp); err != nil {
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}
	log.WithValues("agent_control_plane", req.Name, "agent_control_plane_namespace", req.Namespace)

	// TODO: Check for deletion

	if err := r.reconcileInfraEnv(ctx, log, acp); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentControlPlaneReconciler) reconcileInfraEnv(ctx context.Context, log logr.Logger, acp *controlplanev1.AgentControlPlane) error {
	infraEnv := &aiv1beta1.InfraEnv{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: acp.Namespace, Name: acp.Name}, infraEnv); err != nil {
		if k8serrors.IsNotFound(err) {
			// Create the InfraEnv
			// Reference this InfraEnv by annotation
			infraEnv.Annotations = make(map[string]string)
			infraEnv.Annotations[agentControlPlaneAnnotation] = client.ObjectKeyFromObject(acp).String()
			infraEnv.Spec = aiv1beta1.InfraEnvSpec{
				PullSecretRef: &corev1.LocalObjectReference{
					Name: "test-pull-secret", //TODO: Pass in pull secret name through the acp spec?
				},
			}

			// Add owner ref to ensure GC
			if err := controllerutil.SetOwnerReference(acp, infraEnv, r.Scheme); err != nil {
				log.Error(err, "error setting owner reference on InfraEnv", "infra_env_name", infraEnv.Name)
				return err
			}
			return r.Create(ctx, infraEnv)
		}
		return err
	}

	// InfraEnv exists, check status for ISO download URL
	if infraEnv.Status.ISODownloadURL == "" {
		log.Info("InfraEnv corresponding to the AgentControlPlane  has no image URL available.", "infra_env_name", infraEnv.Name)
		return nil
	}
	// TODO: Set ISO download URL on the MachineTemplate

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1.AgentControlPlane{}).
		Owns(&clusterv1.Machine{}).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterToAgentControlPlane),
		).
		Watches(
			&aiv1beta1.InfraEnv{},
			handler.EnqueueRequestsFromMapFunc(infraEnvToAgentControlPlane),
		).
		Complete(r)
}

// clusterToAgentControlPlane is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// for AgentControlPlane based on updates to a Cluster.
func clusterToAgentControlPlane(_ context.Context, o client.Object) []ctrl.Request {
	c, ok := o.(*clusterv1.Cluster)
	if !ok {
		return nil
	}

	controlPlaneRef := c.Spec.ControlPlaneRef
	if controlPlaneRef != nil && controlPlaneRef.Kind == agentControlPlaneKind {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: controlPlaneRef.Namespace, Name: controlPlaneRef.Name}}}
	}

	return nil
}

// infraEnvToAgentControlPlane is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// for AgentControlPlane based on updates to an InfraEnv.
func infraEnvToAgentControlPlane(_ context.Context, o client.Object) []ctrl.Request {
	i, ok := o.(*aiv1beta1.InfraEnv)
	if !ok {
		return nil
	}

	if i.GetAnnotations() != nil {
		controlPlaneName := i.GetAnnotations()[agentControlPlaneAnnotation]
		if controlPlaneName == "" {
			return []ctrl.Request{}
		}
		parts := strings.SplitN(controlPlaneName, string(types.Separator), 2)
		if len(parts) > 1 {
			return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: parts[0], Name: parts[1]}}}
		}
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: parts[0]}}}
	}
	return nil
}
