package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestHTTPRouteWatcher_Lifecycle(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var receivedEvents []WebhookPayload

	// mock HTTP server for webhook calls
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed to decode webhook payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		receivedEvents = append(receivedEvents, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer.Close()

	// k8s client
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "my-route",
			Namespace:  "default",
			Generation: 1,
			Annotations: map[string]string{
				triggerAnnotation: "true",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"://example.com"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		Build()

	reconciler := &HTTPRouteWatcher{
		Client:         fakeClient,
		ExternalAPIURL: mockServer.URL,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-route", Namespace: "default"},
	}

	// --- TEST: CREATE ---
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		// first reconcile registers the finalizer
		t.Fatalf("reconcile 1 failed: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		// second reconcile triggers the CREATED api call
		t.Fatalf("reconcile 2 failed: %v", err)
	}

	if len(receivedEvents) != 1 || receivedEvents[0].Event != "CREATED" {
		t.Fatalf("expected 1 created event, got: %+v", receivedEvents)
	}

	// --- TEST: SKIP NOOP ---
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		// status-only update should not trigger api calls
		t.Fatalf("status reconcile failed: %v", err)
	}
	if len(receivedEvents) != 1 {
		t.Fatalf("expected no duplicate call on same generation, got %d events", len(receivedEvents))
	}

	// --- TEST: UPDATE ---
	var updatedRoute gatewayv1.HTTPRoute
	_ = fakeClient.Get(ctx, req.NamespacedName, &updatedRoute)
	updatedRoute.Generation = 2 // API server increments generation
	updatedRoute.Spec.Hostnames = []gatewayv1.Hostname{"://example.com"}
	_ = fakeClient.Update(ctx, &updatedRoute)

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("update reconcile failed: %v", err)
	}

	if len(receivedEvents) != 2 || receivedEvents[1].Event != "UPDATED" {
		t.Fatalf("expected UPDATED event, got %+v", receivedEvents)
	}

	// --- TEST: DELETE ---
	now := metav1.NewTime(time.Now())
	_ = fakeClient.Get(ctx, req.NamespacedName, &updatedRoute)
	updatedRoute.DeletionTimestamp = &now
	_ = fakeClient.Update(ctx, &updatedRoute)

	if err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete reconcile failed: %v", err)
	}

	if len(receivedEvents) != 3 || receivedEvents[2].Event != "DELETED" {
		t.Fatalf("expected DELETED event, got %+v", receivedEvents)
	}
}
