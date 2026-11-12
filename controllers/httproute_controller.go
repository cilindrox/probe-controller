package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	triggerAnnotation    = ""
	lastSyncedAnnotation = ""
	finalizerName        = ""
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
	if !route.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&route, finalizerName) {
			payload := WebhookPayload{
				Event:     "DELETED",
				Name:      route.Name,
				Namespace: route.Namespace,
			}
			if err := callExternalAPI(payload); err != nil {
				// backoff retry with until external cleanup succeeds
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

	// Check if trigger annotation is enabled
	if route.Annotations[triggerAnnotation] != "true" {
		// If annotation was removed, clean up the finalizer
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
	// TODO: hash spec fields to account for domain changes
	currentGen := strconv.FormatInt(route.Generation, 10)
	lastSyncedGen := route.Annotations[lastSyncedAnnotation]
	if currentGen == lastSyncedGen {
		return ctrl.Result{}, nil
	}

	eventType := "UPDATED"
	if lastSyncedGen == "" {
		eventType = "CREATED"
	}

	payload := WebhookPayload{
		Event:     eventType,
		Name:      route.Name,
		Namespace: route.Namespace,
		Hostnames: route.Spec.Hostnames,
	}

	if err := callExternalAPI(payload); err != nil {
		return ctrl.Result{}, err
	}

	// Record synced generation to avoid re-triggering on status updates
	if route.Annotations == nil {
		route.Annotations = make(map[string]string)
	}
	route.Annotations[lastSyncedAnnotation] = currentGen
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

	resp, err := http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("external api failed with http %d", resp.StatusCode)
	}
	return nil
}

func (r *HTTPRouteWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Complete(r)
}
