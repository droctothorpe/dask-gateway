package daskcluster

import (
	"context"

	"k8s.io/apimachinery/pkg/util/rand"

	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gatewayv1alpha1 "github.com/dask/dask-gateway/dask-gateway-k8s-operator/pkg/apis/gateway/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_daskcluster")

// Add creates a new DaskCluster Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler.
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileDaskCluster{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler.
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("daskcluster-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource DaskCluster.
	err = c.Watch(&source.Kind{Type: &gatewayv1alpha1.DaskCluster{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Pods and requeue the owner DaskCluster.
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &gatewayv1alpha1.DaskCluster{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileDaskCluster implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileDaskCluster{}

// ReconcileDaskCluster reconciles a DaskCluster object
type ReconcileDaskCluster struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver.
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a DaskCluster object and makes changes based on the state read
// and what is in the DaskCluster.Spec
// Note: the Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileDaskCluster) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger := log
	reqLogger.Info("Reconciling DaskCluster")

	// Fetch the DaskCluster instance.
	cr := &gatewayv1alpha1.DaskCluster{}
	err := r.client.Get(context.TODO(), request.NamespacedName, cr)

	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, err
	}

	// Fetch scheduler.
	scheduler := newSchedulerFromTemplate(cr)
	if err := controllerutil.SetControllerReference(cr, scheduler, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	foundScheduler := &corev1.Pod{}
	err = r.client.Get(
		context.TODO(),
		types.NamespacedName{Name: scheduler.Name, Namespace: scheduler.Namespace},
		foundScheduler,
	)

	// Break early and delete if active is set to false.
	if !cr.Spec.Active {
		switch {
		case err != nil && errors.IsNotFound(err):
			return reconcile.Result{}, nil
		case err != nil:
			return reconcile.Result{}, err
		default:
			reqLogger.Info("Deleting scheduler and workers")

			// Delete scheduler, which deletes the workers as well since they're children of the scheduler.
			err := r.client.Delete(context.TODO(), foundScheduler)

			switch {
			case err != nil && errors.IsNotFound(err):
				return reconcile.Result{}, nil
			case err != nil:
				return reconcile.Result{}, err
			default:
				return reconcile.Result{}, nil
			}
		}
	}

	// If not found, create scheduler
	switch {
	case err != nil && errors.IsNotFound(err):
		reqLogger.Info(
			"Creating a new Scheduler",
			"Scheduler.Namespace",
			"Scheduler.Name",
			scheduler.Namespace,
			scheduler.Name,
		)

		err := r.client.Create(context.TODO(), scheduler)
		if err != nil {
			return reconcile.Result{}, err
		}

		// Trigger reconcile to ensure that we have a foundScheduler so that we can pass it as an owner reference to
		// the worker pods.
		return reconcile.Result{}, nil
	case err != nil:
		return reconcile.Result{}, err
	default:
		// If found, don't create scheduler.
		reqLogger.Info(
			"Scheduler already exists",
			"Scheduler.Namespace",
			"Scheduler.Name",
			scheduler.Namespace,
			scheduler.Name,
		)
	}

	// Fetch workers
	// TODO: Evaluate encapsulating some of this logic.
	worker := newWorkerFromTemplate(cr, foundScheduler)
	labelMap := worker.ObjectMeta.Labels
	labelSelector := labels.SelectorFromSet(labelMap)
	listOps := &client.ListOptions{
		Namespace:     cr.Namespace,
		LabelSelector: labelSelector,
	}
	podList := &corev1.PodList{}

	reqLogger.Info("Fetching workers")

	if err := r.client.List(context.TODO(), podList, listOps); err != nil {
		reqLogger.Error(err, "Failed to retrieve pods")
		return reconcile.Result{}, err
	}

	// Now fork logic based on scaling requirements.
	diff := int(cr.Spec.Worker.Replicas) - len(podList.Items)

	switch {
	// Not enough replicas.
	// Confirmed that this does in fact cover us in the event of external pod termination.
	case diff > 0:
		for diff > 0 {
			reqLogger.Info("Creating worker abstraction")

			worker := newWorkerFromTemplate(cr, foundScheduler)
			// This is required to solve a bug in which the service account is being injected to the worker before
			// any calls to client.Create are made. As a result of the injection, the create fails and the
			// reconciliation needs to start from scratch. I will test this against our dev cluster without the
			// subsequent line. If it still fails, I'll file an issue with Kubernetes.
			worker.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{}

			reqLogger.Info("Creating worker")

			if err := r.client.Create(context.TODO(), worker); err != nil {
				return reconcile.Result{}, err
			}

			diff--
		}
	// More workers than replicas.
	case diff < 0:
		// Delete workers that are pending.
		// TODO: Test this.
		for _, pod := range podList.Items {
			if pod.Status.Phase == "pending" {
				if err := r.client.Delete(context.TODO(), &pod); err != nil {
					return reconcile.Result{}, err
				}
			}
		}
	}

	return reconcile.Result{}, nil
}

// newSchedulerFromTemplate constructs a scheduler pod using the scheduler pod template from the custom resource.
func newSchedulerFromTemplate(cr *gatewayv1alpha1.DaskCluster) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-scheduler",
			Namespace: cr.Namespace,
		},
		Spec: cr.Spec.Scheduler.Template.Spec,
	}
}

// newWorkerFromTemplate constructs a worker pod using the scheduler pod template from the custom resource.
func newWorkerFromTemplate(cr *gatewayv1alpha1.DaskCluster, foundScheduler *corev1.Pod) *corev1.Pod {
	labels := map[string]string{
		"app": cr.Name + "worker",
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-worker-" + rand.String(6),
			Namespace: cr.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(foundScheduler, corev1.SchemeGroupVersion.WithKind("Pod")),
			},
		},
		Spec: cr.Spec.Worker.Template.Spec,
	}
}