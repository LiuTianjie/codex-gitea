package webhook

import (
	"context"
	"io"
	"net/http"

	"github.com/turning4th/codex-gitea/internal/model"
)

// Gitea webhook headers.
const (
	headerEvent     = "X-Gitea-Event"
	headerEventType = "X-Gitea-Event-Type"
	headerDelivery  = "X-Gitea-Delivery"
	headerSignature = "X-Gitea-Signature"
)

// OnEventFunc is the upstream callback invoked with a parsed, verified event.
// It is expected to be quick (e.g. an enqueue). The handler returns 200 to
// Gitea immediately after it returns; downstream work happens out of band.
type OnEventFunc func(context.Context, *model.WebhookEvent) error
type SecretFunc func() string

// Handler verifies and parses incoming Gitea webhooks, then forwards the
// normalized event to OnEvent. It does not perform any downstream processing.
type Handler struct {
	secret     string
	secretFunc SecretFunc
	OnEvent    OnEventFunc
}

// NewHandler builds a Handler with the given webhook secret and optional
// OnEvent callback. If onEvent is omitted, verified events are parsed and
// acknowledged with 200 but otherwise ignored.
func NewHandler(secret string, onEvent ...OnEventFunc) *Handler {
	h := &Handler{secret: secret}
	if len(onEvent) > 0 {
		h.OnEvent = onEvent[0]
	}
	return h
}

// NewHandlerWithSecretFunc builds a Handler that reads the webhook secret for
// each request so console-updated settings take effect without restarting.
func NewHandlerWithSecretFunc(secretFunc SecretFunc, onEvent ...OnEventFunc) *Handler {
	h := &Handler{secretFunc: secretFunc}
	if len(onEvent) > 0 {
		h.OnEvent = onEvent[0]
	}
	return h
}

func (h *Handler) currentSecret() string {
	if h.secretFunc != nil {
		return h.secretFunc()
	}
	return h.secret
}

// Routes returns an http.Handler that serves /webhook and /healthz.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", h.handleWebhook)
	mux.HandleFunc("/healthz", h.handleHealthz)
	return mux
}

// ServeHTTP lets Handler itself satisfy http.Handler by delegating to Routes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Routes().ServeHTTP(w, r)
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the raw body first: needed both for signature verification and to
	// persist as WebhookEvent.Raw before any JSON decoding.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	// Verify the HMAC-SHA256 signature over the raw body (constant-time).
	if !Verify(body, r.Header.Get(headerSignature), h.currentSecret()) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get(headerEventType)
	if eventType == "" {
		eventType = r.Header.Get(headerEvent)
	}
	// Gitea's "Test Delivery" action always sends a synthetic push event,
	// even when the hook is configured only for pull requests and comments.
	// Acknowledge that connectivity probe without creating a review job.
	if eventType == "push" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}
	ev, err := Parse(eventType, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ev.DeliveryID = r.Header.Get(headerDelivery)

	// Forward to the upstream callback. It is expected to be a quick handoff
	// (e.g. enqueue); we acknowledge with 200 immediately afterward.
	if h.OnEvent != nil {
		if err := h.OnEvent(r.Context(), ev); err != nil {
			http.Error(w, "event handling failed", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}
