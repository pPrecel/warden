package admission

import (
	"context"
	"encoding/json"
	"github.com/kyma-project/warden/internal/validate"
	"github.com/kyma-project/warden/pkg"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"net/http"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"time"
)

const (
	DefaultingPath = "/defaulting/pods"
)

const PodType = "Pod"

type DefaultingWebHook struct {
	validationSvc validate.PodValidator
	timeout       time.Duration
	client        k8sclient.Client
	decoder       *admission.Decoder
	logger        *zap.SugaredLogger
}

func NewDefaultingWebhook(client k8sclient.Client, ValidationSvc validate.PodValidator, timeout time.Duration, logger *zap.SugaredLogger) *DefaultingWebHook {
	return &DefaultingWebHook{
		client:        client,
		validationSvc: ValidationSvc,
		logger:        logger,
		timeout:       timeout,
	}
}

func (w *DefaultingWebHook) Handle(ctx context.Context, req admission.Request) admission.Response {
	return w.handleWithLogger(ctx, req)
}

func (w *DefaultingWebHook) handleWithLogger(ctx context.Context, req admission.Request) admission.Response {
	loggerWithReqId := w.logger.With("req-id", req.UID)
	ctxLogger := context.WithValue(ctx, "log", loggerWithReqId)

	resp := w.handleWithTimeMeasure(ctxLogger, req)
	return resp
}

func (w *DefaultingWebHook) handleWithTimeMeasure(ctx context.Context, req admission.Request) admission.Response {
	startTime := time.Now()
	logger := (ctx.Value("log")).(*zap.SugaredLogger)
	logger.Debug("request handling started")
	defer func(startTime time.Time) {
		duration := time.Now().Sub(startTime)
		w.logger.Debugw("request handling finished", "exec-time", duration)
	}(startTime)

	resp := w.handleWithTimeout(ctx, req)
	return resp
}

func (w *DefaultingWebHook) handleWithTimeout(ctx context.Context, req admission.Request) admission.Response {
	ctxTimeout, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	var resp admission.Response
	done := make(chan bool)
	go func() {
		resp = w.handle(ctxTimeout, req)
		done <- true
	}()

	select {
	case <-done:
	case <-ctxTimeout.Done():
		if err := ctxTimeout.Err(); err != nil {
			return admission.Errored(http.StatusRequestTimeout, errors.Wrapf(err, "request exceeded desired timeout: %s", w.timeout.String()))
		}
	}
	return resp
}

func (w *DefaultingWebHook) handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Kind.Kind != PodType {
		return admission.Errored(http.StatusBadRequest,
			errors.Errorf("Invalid request kind:%s, expected:%s", req.Kind.Kind, PodType))
	}

	pod := &corev1.Pod{}
	if err := w.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	ns := &corev1.Namespace{}
	if err := w.client.Get(ctx, k8sclient.ObjectKey{Name: pod.Namespace}, ns); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	result, err := w.validationSvc.ValidatePod(ctx, pod, ns)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if result == validate.NoAction {
		return admission.Allowed("validation is not enabled for pod")
	}

	labeledPod := labelPod(result, pod)
	fBytes, err := json.Marshal(labeledPod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	w.logger.Infof("pod was validated: %s, %s", pod.ObjectMeta.GetName(), pod.ObjectMeta.GetNamespace())
	return admission.PatchResponseFromRaw(req.Object.Raw, fBytes)
}

func (w *DefaultingWebHook) InjectDecoder(decoder *admission.Decoder) error {
	w.decoder = decoder
	return nil
}

func labelPod(result validate.ValidationResult, pod *corev1.Pod) *corev1.Pod {
	labelToApply := LabelForValidationResult(result)
	if labelToApply == "" {
		return pod
	}
	labeledPod := pod.DeepCopy()
	if labeledPod.Labels == nil {
		labeledPod.Labels = map[string]string{}
	}

	labeledPod.Labels[pkg.PodValidationLabel] = labelToApply
	return labeledPod
}

func LabelForValidationResult(result validate.ValidationResult) string {
	switch result {
	case validate.NoAction:
		return ""
	case validate.Invalid:
		return pkg.ValidationStatusReject
	case validate.Valid:
		return pkg.ValidationStatusSuccess
	case validate.ServiceUnavailable:
		return pkg.ValidationStatusPending
	default:
		return pkg.ValidationStatusPending
	}
}
