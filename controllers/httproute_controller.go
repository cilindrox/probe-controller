package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	triggerAnnotation  = "probe-controller.local/register"
	checksumAnnotation = "probe-controller.local/checksum"
	finalizerName      = "probe-controller.local/finalizer"
)

type HTTPRouteWatcher struct {
	client.Client
	ExternalAPIURL string
	APIToken       string
	HTTPClient     *http.Client
}

type WebhookPayload struct {
	Event     string               `json:"event"`
	Name      string               `json:"name"`
	Namespace string               `json:"namespace"`
	Hostnames []gatewayv1.Hostname `json:"hostnames,omitempty"`
}

func (r *HTTPRouteWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Delete
	if !route.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&route, finalizerName) {
			payload := WebhookPayload{
				Event:     "DELETED",
				Name:      route.Name,
				Namespace: route.Namespace,
			}
			if err := r.callExternalAPI(ctx, payload); err != nil {
				// TODO(gfestari): backoff retry with until external cleanup succeeds
				return ctrl.Result{}, err
			}

			// Remove finalizer so k8s can complete deletion
			controllerutil.RemoveFinalizer(&route, finalizerName)
			if err := r.Update(ctx, &route); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// If annotation was removed, clean up the finalizer
	if route.Annotations[triggerAnnotation] != "true" {
		if controllerutil.ContainsFinalizer(&route, finalizerName) {
			controllerutil.RemoveFinalizer(&route, finalizerName)
			return ctrl.Result{}, r.Update(ctx, &route)
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is registered before syncing
	if !controllerutil.ContainsFinalizer(&route, finalizerName) {
		controllerutil.AddFinalizer(&route, finalizerName)
		if err := r.Update(ctx, &route); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Prevent dupe API calls on status-only updates
	currentHash, err := computeSpecHash(route.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}

	lastSyncedHash := route.Annotations[checksumAnnotation]
	if currentHash == lastSyncedHash {
		return ctrl.Result{}, nil
	}

	eventType := "UPDATED"
	if lastSyncedHash == "" {
		eventType = "CREATED"
	}

	payload := WebhookPayload{
		Event:     eventType,
		Name:      route.Name,
		Namespace: route.Namespace,
		Hostnames: route.Spec.Hostnames,
	}

	if err := r.callExternalAPI(ctx, payload); err != nil {
		return ctrl.Result{}, err
	}

	// Record synced generation to avoid re-triggering on status updates
	if route.Annotations == nil {
		route.Annotations = make(map[string]string)
	}
	route.Annotations[checksumAnnotation] = currentHash
	if err := r.Update(ctx, &route); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *HTTPRouteWatcher) callExternalAPI(ctx context.Context, payload WebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ExternalAPIURL, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	if r.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIToken)
	}

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("external api failed with http %d", resp.StatusCode)
	}
	return nil
}

// encode payload and return a 16-char hex digest
func computeSpecHash(spec gatewayv1.HTTPRouteSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("failed to marshal HTTPRoute spec: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]), nil
}

func (r *HTTPRouteWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Complete(r)
}
