/*
Copyright 2023.

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

package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/example/memcached-operator/api/v1alpha1"
)

const codeserverFinalizer = "cache.example.com/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableCodeserver represents the status of the Deployment reconciliation
	typeAvailableCodeserver = "Available"
	// typeDegradedCodeserver represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedCodeserver = "Degraded"
)

// CodeserverReconciler reconciles a Codeserver object
type CodeserverReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=cache.example.com,resources=codeservers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.example.com,resources=codeservers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.example.com,resources=codeservers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *CodeserverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Codeserver instance
	// The purpose is check if the Custom Resource for the Kind Codeserver
	// is applied on the cluster if not we return nil to stop the reconciliation
	codeserver := &cachev1alpha1.Codeserver{}
	err := r.Get(ctx, req.NamespacedName, codeserver)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("codeserver resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get codeserver")
		return ctrl.Result{}, err
	}

	// Let's just set the status as Unknown when no status are available
	if codeserver.Status.Conditions == nil || len(codeserver.Status.Conditions) == 0 {
		meta.SetStatusCondition(&codeserver.Status.Conditions, metav1.Condition{Type: typeAvailableCodeserver, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, codeserver); err != nil {
			log.Error(err, "Failed to update Codeserver status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the codeserver Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, codeserver); err != nil {
			log.Error(err, "Failed to re-fetch codeserver")
			return ctrl.Result{}, err
		}
	}

	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(codeserver, codeserverFinalizer) {
		log.Info("Adding Finalizer for Codeserver")
		if ok := controllerutil.AddFinalizer(codeserver, codeserverFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, codeserver); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Check if the Codeserver instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isCodeserverMarkedToBeDeleted := codeserver.GetDeletionTimestamp() != nil
	if isCodeserverMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(codeserver, codeserverFinalizer) {
			log.Info("Performing Finalizer Operations for Codeserver before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&codeserver.Status.Conditions, metav1.Condition{Type: typeDegradedCodeserver,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", codeserver.Name)})

			if err := r.Status().Update(ctx, codeserver); err != nil {
				log.Error(err, "Failed to update Codeserver status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForCodeserver(codeserver)

			// TODO(user): If you add operations to the doFinalizerOperationsForCodeserver method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the codeserver Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, codeserver); err != nil {
				log.Error(err, "Failed to re-fetch codeserver")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&codeserver.Status.Conditions, metav1.Condition{Type: typeDegradedCodeserver,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", codeserver.Name)})

			if err := r.Status().Update(ctx, codeserver); err != nil {
				log.Error(err, "Failed to update Codeserver status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Codeserver after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(codeserver, codeserverFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Codeserver")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, codeserver); err != nil {
				log.Error(err, "Failed to remove finalizer for Codeserver")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if the deployment already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: codeserver.Name, Namespace: codeserver.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new deployment
		dep, err := r.deploymentForCodeserver(codeserver)
		if err != nil {
			log.Error(err, "Failed to define new Deployment resource for Codeserver")

			// The following implementation will update the status
			meta.SetStatusCondition(&codeserver.Status.Conditions, metav1.Condition{Type: typeAvailableCodeserver,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Deployment for the custom resource (%s): (%s)", codeserver.Name, err)})

			if err := r.Status().Update(ctx, codeserver); err != nil {
				log.Error(err, "Failed to update Codeserver status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Deployment",
			"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new Deployment",
				"Deployment.Namespace", dep.Namespace, "Deployment.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// Deployment created successfully
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get Deployment")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The CRD API is defining that the Codeserver type, have a CodeserverSpec.Size field
	// to set the quantity of Deployment instances is the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment size is the same as defined
	// via the Size spec of the Custom Resource which we are reconciling.
	port := codeserver.Spec.ContainerPort
	if found.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != port {
		found.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort = port
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Failed to update Deployment",
				"Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)

			// Re-fetch the codeserver Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, codeserver); err != nil {
				log.Error(err, "Failed to re-fetch codeserver")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&codeserver.Status.Conditions, metav1.Condition{Type: typeAvailableCodeserver,
				Status: metav1.ConditionFalse, Reason: "Updating port number",
				Message: fmt.Sprintf("Failed to update the port number for the custom resource (%s): (%s)", codeserver.Name, err)})

			if err := r.Status().Update(ctx, codeserver); err != nil {
				log.Error(err, "Failed to update Codeserver status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	// The following implementation will update the status
	meta.SetStatusCondition(&codeserver.Status.Conditions, metav1.Condition{Type: typeAvailableCodeserver,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with port number %d created successfully", codeserver.Name, port)})

	if err := r.Status().Update(ctx, codeserver); err != nil {
		log.Error(err, "Failed to update Codeserver status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// finalizeCodeserver will perform the required operations before delete the CR.
func (r *CodeserverReconciler) doFinalizerOperationsForCodeserver(cr *cachev1alpha1.Codeserver) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// deploymentForCodeserver returns a Codeserver Deployment object
func (r *CodeserverReconciler) deploymentForCodeserver(codeserver *cachev1alpha1.Codeserver) (*appsv1.Deployment, error) {
	ls := labelsForCodeserver(codeserver.Name)

	image, err := imageForCodeserver()
	if err != nil {
		return nil, err
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      codeserver.Name,
			Namespace: codeserver.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &[]bool{true}[0],
						RunAsUser:    &[]int64{1000}[0],
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Image: image,
						Name:  "codeserver",
						SecurityContext: &corev1.SecurityContext{
							Privileged:               &[]bool{false}[0],
							AllowPrivilegeEscalation: &[]bool{false}[0],
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{
									"ALL",
								},
							},
						},
						Ports: []corev1.ContainerPort{{
							ContainerPort: codeserver.Spec.ContainerPort,
							Name:          "codeserver",
						}},
					}},
				},
			},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(codeserver, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// labelsForCodeserver returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForCodeserver(name string) map[string]string {
	var imageTag string
	image, err := imageForCodeserver()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "Codeserver",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "codeserver-operator",
		"app.kubernetes.io/created-by": "codeserver-operator",
	}
}

func imageForCodeserver() (string, error) {
	var imageEnvVar = "CODESERVER_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("environment variable %s not set", imageEnvVar)
	}
	return image, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CodeserverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Codeserver{}).
		Owns(&appsv1.Deployment{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}
