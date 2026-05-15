package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/pandada8/extended-ack-exteneded-network-controller/internal/natgw"
)

const (
	AnnotationPodNATEIP       = "network-x.alibabacloud-x.com/pod-nat-eip"
	AnnotationPodNATGatewayID = "network-x.alibabacloud-x.com/pod-nat-gateway-id"
	AnnotationSNATEntryID     = "network-x.alibabacloud-x.com/pod-nat-snat-entry-id"
	AnnotationSNATTableID     = "network-x.alibabacloud-x.com/pod-nat-snat-table-id"
	AnnotationSNATSourceCIDR  = "network-x.alibabacloud-x.com/pod-nat-source-cidr"
	AnnotationLastRequestID   = "network-x.alibabacloud-x.com/pod-nat-last-request-id"

	ConditionNATGatewayEgressReady corev1.PodConditionType = "network-x.alibabacloud-x.com/NATGatewayEgressReady"
	FinalizerName                                          = "network-x.alibabacloud-x.com/pod-nat-eip-finalizer"

	requeueShort = 5 * time.Second
	requeueLong  = 15 * time.Second
)

type PodNATEIPReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	NATClient natgw.Client
	Recorder  record.EventRecorder
}

func (r *PodNATEIPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("pod-nat-eip-controller")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return hasManagedAnnotations(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return hasManagedAnnotations(e.ObjectNew) || hasManagedAnnotations(e.ObjectOld)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return hasManagedAnnotations(e.Object)
			},
		})).
		Complete(r)
}

func (r *PodNATEIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	config, managed := podConfigFromAnnotations(&pod)
	if !managed {
		if controllerutil.ContainsFinalizer(&pod, FinalizerName) {
			if err := r.removeFinalizer(ctx, &pod); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if err := validatePodConfig(config); err != nil {
		logger.Error(err, "invalid pod NAT annotations")
		setErr := r.setCondition(ctx, &pod, metav1.ConditionFalse, "InvalidAnnotation", err.Error())
		return ctrl.Result{}, firstError(err, setErr)
	}

	if !pod.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &pod, config)
	}

	if !controllerutil.ContainsFinalizer(&pod, FinalizerName) {
		if err := r.addFinalizer(ctx, &pod); err != nil {
			return ctrl.Result{}, err
		}
	}

	if pod.Status.PodIP == "" {
		if err := r.setCondition(ctx, &pod, metav1.ConditionFalse, "PendingPodIP", "waiting for pod IP allocation"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	if !hasReadyCondition(&pod) {
		if err := r.setCondition(ctx, &pod, metav1.ConditionFalse, "EnsuringNATRule", fmt.Sprintf("ensuring NAT rule for pod IP %s on NAT gateway %s", pod.Status.PodIP, config.NATGatewayID)); err != nil {
			return ctrl.Result{}, err
		}
	}

	entry, err := r.NATClient.EnsureSNATEntry(ctx, natgw.EnsureSNATEntryRequest{
		NATGatewayID: config.NATGatewayID,
		SourceCIDR:   pod.Status.PodIP + "/32",
		EIP:          config.EIP,
		EntryName:    snatEntryName(&pod),
	})
	if err != nil {
		message := fmt.Sprintf("failed to ensure NAT rule for pod IP %s on NAT gateway %s: %v", pod.Status.PodIP, config.NATGatewayID, err)
		condErr := r.setCondition(ctx, &pod, metav1.ConditionFalse, "NATRuleCreateFailed", message)
		return ctrl.Result{RequeueAfter: requeueLong}, firstError(err, condErr)
	}

	if entry.Status != natgw.SNATEntryStatusAvailable {
		if err := r.syncNATAnnotations(ctx, &pod, entry); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setCondition(ctx, &pod, metav1.ConditionFalse, "WaitingNATRuleAvailable", fmt.Sprintf("NAT rule %s is in %s state", entry.ID, entry.Status)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	if err := r.syncNATAnnotations(ctx, &pod, entry); err != nil {
		return ctrl.Result{}, err
	}

	message := fmt.Sprintf("NAT rule %s routes %s via %s on %s", entry.ID, pod.Status.PodIP+"/32", config.EIP, config.NATGatewayID)
	wasReady := hasReadyCondition(&pod)
	if err := r.setCondition(ctx, &pod, metav1.ConditionTrue, "NATRuleReady", message); err != nil {
		return ctrl.Result{}, err
	}
	if !wasReady {
		r.Recorder.Event(&pod, corev1.EventTypeNormal, "NATRuleReady", message)
	}

	return ctrl.Result{}, nil
}

func (r *PodNATEIPReconciler) reconcileDelete(ctx context.Context, pod *corev1.Pod, config podNATConfig) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pod, FinalizerName) {
		return ctrl.Result{}, nil
	}

	if pod.Status.PodIP != "" {
		if err := r.NATClient.DeleteSNATEntry(ctx, natgw.DeleteSNATEntryRequest{
			NATGatewayID: config.NATGatewayID,
			SNATTableID:  annotationValue(pod, AnnotationSNATTableID),
			SNATEntryID:  annotationValue(pod, AnnotationSNATEntryID),
			SourceCIDR:   pod.Status.PodIP + "/32",
			EIP:          config.EIP,
		}); err != nil {
			condErr := r.setCondition(ctx, pod, metav1.ConditionFalse, "NATRuleDeleteFailed", err.Error())
			if condErr != nil {
				return ctrl.Result{}, firstError(err, condErr)
			}
			return ctrl.Result{RequeueAfter: requeueLong}, err
		}
	}

	if err := r.removeFinalizer(ctx, pod); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *PodNATEIPReconciler) addFinalizer(ctx context.Context, pod *corev1.Pod) error {
	base := pod.DeepCopy()
	controllerutil.AddFinalizer(pod, FinalizerName)
	return r.Patch(ctx, pod, client.MergeFrom(base))
}

func (r *PodNATEIPReconciler) removeFinalizer(ctx context.Context, pod *corev1.Pod) error {
	base := pod.DeepCopy()
	controllerutil.RemoveFinalizer(pod, FinalizerName)
	return r.Patch(ctx, pod, client.MergeFrom(base))
}

func (r *PodNATEIPReconciler) setCondition(ctx context.Context, pod *corev1.Pod, status metav1.ConditionStatus, reason, message string) error {
	latest := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, latest); err != nil {
		return err
	}

	base := latest.DeepCopy()
	condition := corev1.PodCondition{
		Type:               ConditionNATGatewayEgressReady,
		Status:             corev1.ConditionStatus(status),
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	setPodCondition(&latest.Status.Conditions, condition)

	if conditionsEqual(base.Status.Conditions, latest.Status.Conditions) {
		return nil
	}

	return r.Status().Patch(ctx, latest, client.MergeFrom(base))
}

func (r *PodNATEIPReconciler) syncNATAnnotations(ctx context.Context, pod *corev1.Pod, entry *natgw.SNATEntry) error {
	latest := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, latest); err != nil {
		return err
	}

	base := latest.DeepCopy()
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[AnnotationSNATEntryID] = entry.ID
	latest.Annotations[AnnotationSNATTableID] = entry.TableID
	latest.Annotations[AnnotationSNATSourceCIDR] = entry.SourceCIDR
	if entry.RequestID != "" {
		latest.Annotations[AnnotationLastRequestID] = entry.RequestID
	}

	if stringMapsEqual(base.Annotations, latest.Annotations) {
		return nil
	}

	if err := r.Patch(ctx, latest, client.MergeFrom(base)); err != nil {
		return err
	}
	pod.Annotations = latest.Annotations
	return nil
}

func setPodCondition(conditions *[]corev1.PodCondition, condition corev1.PodCondition) {
	for i := range *conditions {
		if (*conditions)[i].Type != condition.Type {
			continue
		}
		if (*conditions)[i].Status == condition.Status {
			condition.LastTransitionTime = (*conditions)[i].LastTransitionTime
		}
		(*conditions)[i] = condition
		return
	}
	*conditions = append(*conditions, condition)
}

func conditionsEqual(left, right []corev1.PodCondition) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Type != right[i].Type || left[i].Status != right[i].Status || left[i].Reason != right[i].Reason || left[i].Message != right[i].Message {
			return false
		}
	}
	return true
}

type podNATConfig struct {
	EIP          string
	NATGatewayID string
}

func podConfigFromAnnotations(pod *corev1.Pod) (podNATConfig, bool) {
	annotations := pod.GetAnnotations()
	if annotations == nil {
		return podNATConfig{}, false
	}

	eip := strings.TrimSpace(annotations[AnnotationPodNATEIP])
	natGatewayID := strings.TrimSpace(annotations[AnnotationPodNATGatewayID])
	if eip == "" && natGatewayID == "" {
		return podNATConfig{}, false
	}

	return podNATConfig{EIP: eip, NATGatewayID: natGatewayID}, true
}

func validatePodConfig(config podNATConfig) error {
	if config.EIP == "" {
		return fmt.Errorf("missing annotation %s", AnnotationPodNATEIP)
	}
	if config.NATGatewayID == "" {
		return fmt.Errorf("missing annotation %s", AnnotationPodNATGatewayID)
	}
	if errors := validation.IsQualifiedName(string(ConditionNATGatewayEgressReady)); len(errors) > 0 {
		return fmt.Errorf("invalid condition type %s: %s", ConditionNATGatewayEgressReady, strings.Join(errors, ","))
	}
	return nil
}

func snatEntryName(pod *corev1.Pod) string {
	name := fmt.Sprintf("pod-%s-%s", pod.Namespace, pod.Name)
	if len(name) > 128 {
		return name[:128]
	}
	return name
}

func hasReadyCondition(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == ConditionNATGatewayEgressReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasManagedAnnotations(obj client.Object) bool {
	if obj == nil {
		return false
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return false
	}
	return annotations[AnnotationPodNATEIP] != "" || annotations[AnnotationPodNATGatewayID] != ""
}

func annotationValue(pod *corev1.Pod, key string) string {
	if pod.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(pod.Annotations[key])
}

func stringMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if right[key] != leftValue {
			return false
		}
	}
	return true
}

func firstError(primary, secondary error) error {
	if primary != nil {
		return primary
	}
	return secondary
}
