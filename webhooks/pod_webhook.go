package webhooks

// import (
// 	"context"
// 	"net/http"

// 	admissionv1 "k8s.io/api/admission/v1"
// 	corev1 "k8s.io/api/core/v1"
// 	"k8s.io/apimachinery/pkg/types"
// 	ctrl "sigs.k8s.io/controller-runtime"
// 	"sigs.k8s.io/controller-runtime/pkg/client"
// 	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
// )

// type PodQuotaWebhook struct {
// 	Client  client.Client
// 	Decoder *admission.Decoder
// }

// func (w *PodQuotaWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
// 	if req.Operation != admissionv1.Create {
// 		return admission.Allowed("")
// 	}

// 	pod := &corev1.Pod{}
// 	if err := w.Decoder.Decode(req, pod); err != nil {
// 		return admission.Errored(http.StatusBadRequest, err)
// 	}

// 	// Load ResourceQuota for this namespace
// 	var rqList corev1.ResourceQuotaList
// 	if err := w.Client.List(ctx, &rqList, client.InNamespace(req.Namespace)); err != nil {
// 		return admission.Errored(http.StatusInternalServerError, err)
// 	}
// 	if len(rqList.Items) == 0 {
// 		return admission.Allowed("") // no quota → allow pod
// 	}
// 	rq := rqList.Items[0] // assume 1 quota per namespace

// 	// Sum Pod requests
// 	var reqCPU, reqMem, reqGPU int64
// 	for _, c := range pod.Spec.Containers {
// 		if cpu := c.Resources.Requests[corev1.ResourceCPU]; cpu.Value() > 0 {
// 			reqCPU += cpu.MilliValue()
// 		}
// 		if mem := c.Resources.Requests[corev1.ResourceMemory]; mem.Value() > 0 {
// 			reqMem += mem.Value()
// 		}
// 		if gpu := c.Resources.Requests["nvidia.com/gpu"]; gpu.Value() > 0 {
// 			reqGPU += gpu.Value()
// 		}
// 	}

// 	usedCPU := rq.Status.Used[corev1.ResourceCPU].MilliValue()
// 	hardCPU := rq.Spec.Hard[corev1.ResourceCPU].MilliValue()

// 	usedMem := rq.Status.Used[corev1.ResourceMemory].Value()
// 	hardMem := rq.Spec.Hard[corev1.ResourceMemory].Value()

// 	usedGPU := rq.Status.Used["nvidia.com/gpu"].Value()
// 	hardGPU := rq.Spec.Hard["nvidia.com/gpu"].Value()

// 	// If Pod exceeds quota → reject & trigger autoscaler
// 	if usedCPU+reqCPU > hardCPU ||
// 		usedMem+reqMem > hardMem ||
// 		usedGPU+reqGPU > hardGPU {

// 		// Emit event triggering reconcile
// 		// Add an annotation to NamespaceQuota (pattern)
// 		// You can refine this logic
// 		nsqName := "namespacequota-sample"
// 		nsq := types.NamespacedName{Name: nsqName}
// 		var empty struct{}
// 		w.Client.Patch(ctx, &empty, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"scaling.dcn.ssu.ac.kr/trigger":"now"}}}`)))

// 		return admission.Denied("Quota exceeded: autoscaler scaling in progress, retry shortly")
// 	}

// 	return admission.Allowed("")
// }

// func (w *PodQuotaWebhook) InjectDecoder(d *admission.Decoder) error {
// 	w.Decoder = d
// 	return nil
// }

// func SetupWebhook(mgr ctrl.Manager) {
// 	mgr.GetWebhookServer().Register("/validate-v1-pod", &admission.Webhook{
// 		Handler: &PodQuotaWebhook{
// 			Client: mgr.GetClient(),
// 		},
// 	})
// }
