package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const podHousekeeperMarkAnnotation = "pod-housekeeper/marked-for-deletion"
const podHousekeeperExcludeAnnotation = "pod-housekeeper/exclude"
const podHousekeeperFieldManager = "pod-housekeeper-controller"

// controllerOwnerIndexField is used in main.go for indexing, keep it there or move to internal
// const controllerOwnerIndexField = ".metadata.controller" // Let's keep this in main.go for now

// Moved metrics definitions here
var (
	podsMarkedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pod_housekeeper_marked_total",
			Help: "Total number of pods marked for deletion by pod-housekeeper",
		},
		[]string{"namespace"}, // Define labels
	)
	podsDeletedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pod_housekeeper_deleted_total",
			Help: "Total number of pods deleted by pod-housekeeper",
		},
		[]string{"namespace"}, // Define labels
	)
)

// Metrics registration happens here, triggered by the import in main.go
func init() {
	metrics.Registry.MustRegister(podsMarkedCounter)
	metrics.Registry.MustRegister(podsDeletedCounter)
}

// ReconcilePods reconciles Pods based on their start time (Capitalized struct name)
type ReconcilePods struct { // Capitalized struct name
	// Fields need to be public if accessed from main.go, but they are set up there.
	// Let's keep them lowercase for now as they are assigned within main.go
	// If we needed methods on ReconcilePods to access them from elsewhere, they'd need capitalization.
	Client                  client.Client        // Capitalized field name
	MarkDuration            time.Duration        // Capitalized field name
	DeleteDuration          time.Duration        // Capitalized field name
	ExcludedNamespaces      map[string]struct{}  // Capitalized field name
	MaxMarkedPerOwner       int                  // Capitalized field name
	MaxMarkedPerOwnerByKind map[string]int       // Capitalized field name
	CheckExcludeAnnotation  bool                 // Capitalized field name
	Recorder                record.EventRecorder // Capitalized field name
	ExcludeSelf             bool                 // Capitalized field name
	PodName                 string               // Capitalized field name
	PodNamespace            string               // Capitalized field name
}

// Implement reconcile.Reconciler so the controller can reconcile objects
// The assertion needs to use the new capitalized name
var _ reconcile.Reconciler = &ReconcilePods{} // Capitalized struct name

// Reconcile method needs to be capitalized to be accessible by controller-runtime
func (r *ReconcilePods) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) { // Capitalized struct name
	log := log.FromContext(ctx).WithValues("pod", request.NamespacedName)

	// Fetch the Pod from the cache
	pod := &corev1.Pod{}
	err := r.Client.Get(ctx, request.NamespacedName, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Pod not found. Ignoring since object must be deleted")
			return reconcile.Result{}, nil
		}
		log.Error(err, "Failed to get Pod")
		// Add UID to log if available in request (it usually isn't, but try)
		// UID will be available once pod is fetched successfully.
		return reconcile.Result{}, fmt.Errorf("failed to get Pod %s: %w", request.NamespacedName, err)
	}

	// --- Check if this pod is the controller itself ---
	if r.ExcludeSelf && pod.Name == r.PodName && pod.Namespace == r.PodNamespace {
		// Only log if V(1) is enabled to avoid noise for every reconcile of self
		log.V(1).Info("Skipping reconciliation for controller's own pod", "selfPodName", r.PodName, "selfPodNamespace", r.PodNamespace)
		return reconcile.Result{}, nil
	}

	// Add pod UID to logger now that we have the object
	log = log.WithValues("podUID", pod.UID)

	// --- Check Namespace Exclusion FIRST ---
	_, isExcluded := r.ExcludedNamespaces[pod.Namespace]
	log.V(1).Info("Checking namespace exclusion", "namespace", pod.Namespace, "isExcluded", isExcluded)
	if isExcluded {
		log.Info("Pod is in an excluded namespace, skipping reconciliation")
		return reconcile.Result{}, nil
	}

	// --- Check Pod Annotation Exclusion ---
	if r.CheckExcludeAnnotation {
		if pod.Annotations != nil {
			if _, found := pod.Annotations[podHousekeeperExcludeAnnotation]; found {
				log.Info("Pod has exclusion annotation, skipping reconciliation", "annotationKey", podHousekeeperExcludeAnnotation)
				return reconcile.Result{}, nil
			}
		}
		log.V(1).Info("Pod does not have exclusion annotation or checking is disabled", "checkEnabled", r.CheckExcludeAnnotation)
	} // else: Annotation check is disabled

	// Ensure StartTime is not nil before proceeding
	if pod.Status.StartTime == nil {
		log.V(1).Info("Pod start time not set yet, requeueing shortly")
		// Add jitter to fixed requeue
		return reconcile.Result{RequeueAfter: addJitter(15*time.Second, 0.1)}, nil
	}

	// Check if pod has deletion timestamp
	if pod.ObjectMeta.DeletionTimestamp != nil {
		log.Info("Pod is already terminating, skipping")
		return reconcile.Result{}, nil
	}

	now := time.Now()
	startTime := pod.Status.StartTime.Time
	var alreadyMarked bool
	var markedTimestampStr string
	if pod.Annotations != nil {
		markedTimestampStr, alreadyMarked = pod.Annotations[podHousekeeperMarkAnnotation]
	}

	// --- Delete Logic ---
	if alreadyMarked {
		markTimestamp, err := time.Parse(time.RFC3339, markedTimestampStr)
		if err != nil {
			log.Error(err, "Failed to parse mark timestamp from annotation", "annotationValue", markedTimestampStr)
			return reconcile.Result{RequeueAfter: 1 * time.Minute}, nil
		}

		requiredDurationSinceMark := r.DeleteDuration - r.MarkDuration
		if requiredDurationSinceMark < 0 {
			log.Error(fmt.Errorf("calculated negative duration between delete and mark"), "Invalid configuration detected", "markDuration", r.MarkDuration, "deleteDuration", r.DeleteDuration)
			requiredDurationSinceMark = 0
		}

		elapsedSinceMark := now.Sub(markTimestamp)
		if elapsedSinceMark >= requiredDurationSinceMark {
			log.Info("Pod has been marked for longer than the delete interval, attempting deletion", "markTimestamp", markTimestamp.Format(time.RFC3339), "elapsedSinceMark", elapsedSinceMark.String(), "requiredDurationSinceMark", requiredDurationSinceMark.String())
			err = r.Client.Delete(ctx, pod)
			if err != nil {
				if errors.IsNotFound(err) {
					log.Info("Pod already deleted before delete call, likely externally")
					return reconcile.Result{}, nil
				}
				log.Error(err, "Failed to delete Pod")
				// Record warning event on failure
				r.Recorder.Eventf(pod, corev1.EventTypeWarning, "FailedDelete", "Failed to delete pod %s/%s (UID: %s): %v", pod.Namespace, pod.Name, pod.UID, err)
				return reconcile.Result{}, fmt.Errorf("failed to delete Pod %s (%s): %w", request.NamespacedName, pod.UID, err)
			}
			log.Info("Successfully deleted Pod due to age")
			// Record event after successful deletion
			r.Recorder.Eventf(pod, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted pod %s/%s because it exceeded the deletion duration (%s) after being marked at %s", pod.Namespace, pod.Name, r.DeleteDuration.String(), markTimestamp.Format(time.RFC3339))
			// Increment counter with namespace label
			podsDeletedCounter.With(prometheus.Labels{"namespace": pod.Namespace}).Inc()
			return reconcile.Result{}, nil
		} else {
			nextCheckTime := markTimestamp.Add(requiredDurationSinceMark)
			requeueDuration := max(time.Until(nextCheckTime), 0)
			requeueDuration += 2 * time.Second
			// Add jitter to fixed requeue
			jitteredRequeueDuration := addJitter(requeueDuration, 0.1)
			log.V(1).Info("Pod is marked, requeueing for deletion check", "deleteAt", nextCheckTime.Format(time.RFC3339), "requeueAfter", jitteredRequeueDuration.String())
			return reconcile.Result{RequeueAfter: jitteredRequeueDuration}, nil
		}
	}

	// --- Mark Logic ---
	if !alreadyMarked {
		elapsedSinceStart := now.Sub(startTime)
		if elapsedSinceStart >= r.MarkDuration {
			log.Info("Pod age exceeds mark duration, checking owner limits before marking", "elapsedSinceStart", elapsedSinceStart.String(), "markDuration", r.MarkDuration.String())

			ownerRef := metav1.GetControllerOf(pod)
			ownerUID := types.UID("")

			if ownerRef != nil {
				ownerUID = ownerRef.UID
				log = log.WithValues("ownerKind", ownerRef.Kind, "ownerName", ownerRef.Name, "ownerUID", ownerUID)

				siblingPods := &corev1.PodList{}
				// Create a context with a timeout for the List call
				listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel() // Ensure the context resources are released

				// The index field key is defined and used during manager setup in main.go
				// We just use the ownerUID here for the MatchingFields value.
				err := r.Client.List(listCtx, siblingPods, // Use the timeout context here
					client.InNamespace(pod.Namespace),
					// Use the index field key string directly, as defined in main.go
					client.MatchingFields{".metadata.controller": string(ownerUID)},
				)
				if err != nil {
					// Check specifically for context deadline exceeded
					if listCtx.Err() == context.DeadlineExceeded {
						// Log using the known key string
						log.Error(err, "Context deadline exceeded while listing sibling pods for owner check", "ownerKey", ".metadata.controller", "ownerUID", string(ownerUID), "timeout", "10s")
						return reconcile.Result{Requeue: true}, nil // Requeue immediately on timeout
					}
					// Handle other errors
					// Log using the known key string
					log.Error(err, "Failed to list sibling pods using index for owner check", "ownerKey", ".metadata.controller", "ownerUID", string(ownerUID))
					return reconcile.Result{}, fmt.Errorf("failed to list pods by owner %s in namespace %s: %w", ownerUID, pod.Namespace, err)
				}

				markedCount := 0
				for _, sibling := range siblingPods.Items {
					if sibling.UID == pod.UID {
						continue // Skip the current pod itself
					}
					if sibling.Annotations != nil {
						if _, siblingMarked := sibling.Annotations[podHousekeeperMarkAnnotation]; siblingMarked {
							markedCount++
						}
					}
				}

				// Determine the limit for this owner Kind
				ownerKind := ownerRef.Kind
				limit, found := r.MaxMarkedPerOwnerByKind[ownerKind]
				if !found {
					limit = r.MaxMarkedPerOwner // Use default if Kind not specified
					log.V(1).Info("Using default marking limit for owner kind", "ownerKind", ownerKind, "limit", limit)
				} else {
					log.V(1).Info("Using specific marking limit for owner kind", "ownerKind", ownerKind, "limit", limit)
				}

				log.V(1).Info("Checked siblings for marking limit using index", "siblingCount", len(siblingPods.Items)-1, "markedSiblings", markedCount, "limitBeingApplied", limit)

				if markedCount >= limit { // Use the determined limit here
					log.Info("Marking limit reached for owner, requeueing", "ownerKind", ownerKind, "markedCount", markedCount, "limit", limit)
					// Add jitter to fixed requeue
					return reconcile.Result{RequeueAfter: addJitter(3*time.Minute, 0.1)}, nil
				}
			} else {
				log.V(1).Info("Pod has no controller owner, skipping sibling check for marking limit.")
			}

			log.Info("Attempting to mark Pod using Server-Side Apply")

			// --- Server-Side Apply Patch ---
			// Create a minimal Pod object containing only the fields we want to manage.
			applyPod := &corev1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(), // Important for SSA
					Kind:       "Pod",                              // Important for SSA
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      pod.Name,
					Namespace: pod.Namespace,
					Annotations: map[string]string{
						podHousekeeperMarkAnnotation: now.Format(time.RFC3339),
					},
				},
				// No spec or status needed, only the fields we manage.
			}

			// Use Patch with client.Apply options.
			// client.ForceOwnership ensures we take ownership if the field was previously managed differently.
			err = r.Client.Patch(ctx, applyPod, client.Apply, client.FieldOwner(podHousekeeperFieldManager), client.ForceOwnership)

			if err != nil {
				// Conflicts with SSA are generally more specific about field ownership.
				// Requeueing on conflict is still a reasonable strategy.
				if errors.IsConflict(err) {
					log.Info("Conflict applying patch for marking (likely SSA field ownership), requeueing for retry", "error", err)
					return reconcile.Result{Requeue: true}, nil
				} else if errors.IsNotFound(err) {
					log.Info("Pod not found during marking patch, likely deleted externally")
					return reconcile.Result{}, nil
				} else if errors.IsInvalid(err) {
					log.Info("Failed to mark pod due to UID mismatch (likely deleted and recreated), ignoring", "error", err.Error())
					return reconcile.Result{}, nil
				}
				log.Error(err, "Failed to patch Pod when marking")
				// Record warning event on failure (excluding conflict/notfound/invalid which are handled above)
				r.Recorder.Eventf(pod, corev1.EventTypeWarning, "FailedMark", "Failed to patch pod %s/%s (UID: %s) to mark for deletion: %v", pod.Namespace, pod.Name, pod.UID, err)
				return reconcile.Result{}, fmt.Errorf("failed to patch Pod %s (%s) when marking: %w", request.NamespacedName, pod.UID, err)
			}
			log.Info("Successfully marked Pod via Server-Side Apply")
			// Increment counter with namespace label
			podsMarkedCounter.With(prometheus.Labels{"namespace": pod.Namespace}).Inc()
			// Calculate requeue for deletion check
			deleteTime := now.Add(r.DeleteDuration - r.MarkDuration)
			deleteRequeueDuration := max(time.Until(deleteTime), 0)
			deleteRequeueDuration += 2 * time.Second // Add small buffer
			// Add jitter to fixed requeue
			jitteredDeleteRequeueDuration := addJitter(deleteRequeueDuration, 0.1)
			log.V(1).Info("Requeueing after marking for deletion check", "deleteAtApprox", deleteTime.Format(time.RFC3339), "requeueAfter", jitteredDeleteRequeueDuration.String())
			return reconcile.Result{RequeueAfter: jitteredDeleteRequeueDuration}, nil
		} else {
			// Calculate requeue for mark check
			nextCheckTime := startTime.Add(r.MarkDuration)
			requeueDuration := max(time.Until(nextCheckTime), 0)
			requeueDuration += 2 * time.Second // Add small buffer
			// Add jitter to fixed requeue
			jitteredRequeueDuration := addJitter(requeueDuration, 0.1)
			log.V(1).Info("Pod not marked, requeueing for mark check", "markAt", nextCheckTime.Format(time.RFC3339), "requeueAfter", jitteredRequeueDuration.String())
			return reconcile.Result{RequeueAfter: jitteredRequeueDuration}, nil
		}
	}

	log.Info("Reached end of reconcile logic unexpectedly, requeueing after 5 minutes")
	// Add jitter to fixed requeue
	return reconcile.Result{RequeueAfter: addJitter(5*time.Minute, 0.1)}, nil
}

// addJitter adds a random duration up to maxFactor*duration to the input duration.
// This helps prevent multiple controllers from acting simultaneously.
func addJitter(duration time.Duration, maxFactor float64) time.Duration {
	if maxFactor <= 0 {
		return duration
	}
	jitter := time.Duration(rand.Float64() * maxFactor * float64(duration))
	return duration + jitter
}
