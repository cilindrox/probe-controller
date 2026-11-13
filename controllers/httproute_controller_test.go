package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type testEnv struct {
	client         client.Client
	reconciler     *HTTPRouteWatcher
	mu             sync.Mutex
	events         []WebhookPayload
	statusCode     int
	lastAuthHeader string
}

func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()

	env := &testEnv{
		statusCode: http.StatusOK,
	}

	// mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.lastAuthHeader = r.Header.Get("Authorization")
		code := env.statusCode
		env.mu.Unlock()

		if code >= 400 {
			w.WriteHeader(code)
			return
		}

		var payload WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed to decode webhook payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		env.mu.Lock()
		env.events = append(env.events, payload)
		env.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	// k8s client
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)

	env.client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()

	env.reconciler = &HTTPRouteWatcher{
		Client:         env.client,
		ExternalAPIURL: server.URL,
	}

	return env
}

func (e *testEnv) recordedEvents() []WebhookPayload {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]WebhookPayload, len(e.events))
	copy(out, e.events)
	return out
}

func TestHTTPRouteWatcher_Lifecycle(t *testing.T) {
	ctx := context.Background()

	route := &gatewayv1.HTTPRoute{
		Name:      "my-route",
		Namespace: "default",
		Annotations: map[string]string{
			triggerAnnotation: "true",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
		},
	}

	env := newTestEnv(t, route)
	req := ctrl.Request{
		Name:      "my-route",
		Namespace: "default",
	}

	// --- TEST: CREATE ---
	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		// first reconcile registers the finalizer
		t.Fatalf("reconcile 1 failed: %v", err)
	}
	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		// second reconcile triggers the CREATED api call
		t.Fatalf("reconcile 2 failed: %v", err)
	}

	events := env.recordedEvents()
	if len(events) != 1 || events[0].Event != "CREATED" {
		t.Fatalf("expected 1 CREATED event, got: %+v", events)
	}
	if len(events[0].Hostnames) != 1 || events[0].Hostnames[0] != "app.example.com" {
		t.Fatalf("expected hostname app.example.com, got: %+v", events[0].Hostnames)
	}

	// --- TEST: SKIP NOOP ---
	var statusRoute gatewayv1.HTTPRoute
	_ = env.client.Get(ctx, req.NamespacedName, &statusRoute)
	statusRoute.Status.Parents = []gatewayv1.RouteParentStatus{
		{
			ParentRef: gatewayv1.ParentReference{Name: "my-gateway"},
		},
	}
	_ = env.client.Status().Update(ctx, &statusRoute)

	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("status reconcile failed: %v", err)
	}
	events = env.recordedEvents()
	if len(events) != 1 {
		t.Fatalf("expected no duplicate call on same generation, got %d events", len(events))
	}

	// --- TEST: UPDATE ---
	var updatedRoute gatewayv1.HTTPRoute
	_ = env.client.Get(ctx, req.NamespacedName, &updatedRoute)
	updatedRoute.Spec.Hostnames = []gatewayv1.Hostname{"www.example.com"}
	_ = env.client.Update(ctx, &updatedRoute)

	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("update reconcile failed: %v", err)
	}

	events = env.recordedEvents()
	if len(events) != 2 || events[1].Event != "UPDATED" {
		t.Fatalf("expected UPDATED event, got: %+v", events)
	}
	if len(events[1].Hostnames) != 1 || events[1].Hostnames[0] != "www.example.com" {
		t.Fatalf("expected updated hostname www.example.com, got: %+v", events[1].Hostnames)
	}

	// --- TEST: DELETE ---
	_ = env.client.Get(ctx, req.NamespacedName, &updatedRoute)
	if err := env.client.Delete(ctx, &updatedRoute); err != nil {
		t.Fatalf("delete route failed: %v", err)
	}

	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete reconcile failed: %v", err)
	}

	events = env.recordedEvents()
	if len(events) != 3 || events[2].Event != "DELETED" {
		t.Fatalf("expected DELETED event, got: %+v", events)
	}
}

func TestHTTPRouteWatcher_AnnotationRemoved_CleansUpFinalizer(t *testing.T) {
	ctx := context.Background()

	route := &gatewayv1.HTTPRoute{
		Name:       "untracked-route",
		Namespace:  "default",
		Finalizers: []string{finalizerName},
	}

	env := newTestEnv(t, route)
	req := ctrl.Request{
		Name:      "untracked-route",
		Namespace: "default",
	}

	// --- TEST: Remove finalizer without calling external API ---
	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var updated gatewayv1.HTTPRoute
	if err := env.client.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatalf("failed to fetch updated route: %v", err)
	}
	if controllerutil.ContainsFinalizer(&updated, finalizerName) {
		t.Fatalf("expected finalizer %q to be removed, still present: %v", finalizerName, updated.Finalizers)
	}
	if len(env.recordedEvents()) != 0 {
		t.Fatalf("expected 0 API calls when annotation is missing, got: %+v", env.recordedEvents())
	}

	// --- TEST: noop when neither annotation nor finalizer are present ---
	if _, err := env.reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
}

func TestHTTPRouteWatcher_ErrorsAndAuth(t *testing.T) {
	tests := []struct {
		name              string
		route             *gatewayv1.HTTPRoute
		deleteBeforeSync  bool
		apiStatusCode     int
		apiToken          string
		overrideURL       string
		wantErr           bool
		wantFinalizerKept bool
		wantAuthHeader    string
	}{
		{
			name:          "not found route cleanly ignores error",
			route:         nil,
			apiStatusCode: http.StatusOK,
			wantErr:       false,
		},
		{
			name: "API 500 on create returns error and does not stamp generation",
			route: &gatewayv1.HTTPRoute{
				Name:        "fail-create",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			apiStatusCode:     http.StatusInternalServerError,
			wantErr:           true,
			wantFinalizerKept: true,
		},
		{
			name: "API 500 on delete returns error and keeps finalizer",
			route: &gatewayv1.HTTPRoute{
				Name:        "fail-delete",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			deleteBeforeSync:  true,
			apiStatusCode:     http.StatusInternalServerError,
			wantErr:           true,
			wantFinalizerKept: true,
		},
		{
			name: "invalid external URL returns request error",
			route: &gatewayv1.HTTPRoute{
				Name:        "bad-url",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			overrideURL:       "://invalid-url",
			wantErr:           true,
			wantFinalizerKept: true,
		},
		{
			name: "sends Bearer token when APIToken is configured",
			route: &gatewayv1.HTTPRoute{
				Name:        "auth-route",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			apiStatusCode:     http.StatusOK,
			apiToken:          "super-secret-token",
			wantErr:           false,
			wantFinalizerKept: true,
			wantAuthHeader:    "Bearer super-secret-token",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			var env *testEnv
			req := ctrl.Request{
				Name:      "missing-route",
				Namespace: "default",
			}

			if tc.route != nil {
				env = newTestEnv(t, tc.route)
				req.Name = tc.route.Name
				req.Namespace = tc.route.Namespace
			} else {
				env = newTestEnv(t)
			}

			env.statusCode = tc.apiStatusCode
			env.reconciler.APIToken = tc.apiToken
			if tc.overrideURL != "" {
				env.reconciler.ExternalAPIURL = tc.overrideURL
			}

			if tc.deleteBeforeSync && tc.route != nil {
				if err := env.client.Delete(ctx, tc.route); err != nil {
					t.Fatalf("failed to mark route for deletion: %v", err)
				}
			}

			_, err := env.reconciler.Reconcile(ctx, req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Reconcile() error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantAuthHeader != "" && env.lastAuthHeader != tc.wantAuthHeader {
				t.Fatalf("Authorization header = %q, want %q", env.lastAuthHeader, tc.wantAuthHeader)
			}

			if tc.route != nil {
				var current gatewayv1.HTTPRoute
				if err := env.client.Get(ctx, req.NamespacedName, &current); err != nil {
					t.Fatalf("failed to get route after reconcile: %v", err)
				}

				hasFinalizer := controllerutil.ContainsFinalizer(&current, finalizerName)
				if hasFinalizer != tc.wantFinalizerKept {
					t.Fatalf("finalizer present = %v, want %v", hasFinalizer, tc.wantFinalizerKept)
				}

				if tc.wantErr && current.Annotations[checksumAnnotation] != "" {
					t.Fatalf("expected checksumAnnotation not to be stamped on failed API call, got %q", current.Annotations[checksumAnnotation])
				}
			}
		})
	}
}

func TestHTTPRouteWatcher_SetupWithManager(t *testing.T) {
	// mock API server discovery endpoints
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
		case "/apis":
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","groups":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiServer.Close)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("failed to install gatewayv1 scheme: %v", err)
	}

	// disable metrics and probes to avoid port conflicts
	mgr, err := ctrl.NewManager(&rest.Config{Host: apiServer.URL}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	reconciler := &HTTPRouteWatcher{
		Client: mgr.GetClient(),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager() unexpected error = %v", err)
	}
}
