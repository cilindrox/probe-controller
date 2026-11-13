package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
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
	if len(events) != 1 || events[0].Event != eventCreated {
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
	if len(events) != 2 || events[1].Event != eventUpdated {
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
	if len(events) != 3 || events[2].Event != eventDeleted {
		t.Fatalf("expected DELETED event, got: %+v", events)
	}
}

func TestHTTPRouteWatcher_AnnotationRemoved_CleansUpFinalizer(t *testing.T) {
	ctx := context.Background()

	route := &gatewayv1.HTTPRoute{
		Name:       "untracked-route",
		Namespace:  "default",
		Finalizers: []string{finalizerName},
		Annotations: map[string]string{
			checksumAnnotation: "some-previous-hash",
		},
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

	events := env.recordedEvents()
	if len(events) != 1 || events[0].Event != eventDeleted {
		t.Fatalf("expected 1 DELETED event on unregister, got: %+v", events)
	}

	var updated gatewayv1.HTTPRoute
	if err := env.client.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatalf("failed to fetch updated route: %v", err)
	}
	if controllerutil.ContainsFinalizer(&updated, finalizerName) {
		t.Fatalf("expected finalizer %q to be removed, still present: %v", finalizerName, updated.Finalizers)
	}
	if _, exists := updated.Annotations[checksumAnnotation]; exists {
		t.Fatalf("expected %q annotation to be removed", checksumAnnotation)
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
			name: "API 500 on unregister returns error and keeps finalizer",
			route: &gatewayv1.HTTPRoute{
				Name:        "fail-unregister",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{checksumAnnotation: "existing-checksum"},
			},
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
		{
			name: "API 404 on delete succeeds and removes finalizer",
			route: &gatewayv1.HTTPRoute{
				Name:        "already-gone-404",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			deleteBeforeSync:  true,
			apiStatusCode:     http.StatusNotFound,
			wantErr:           false,
			wantFinalizerKept: false,
		},
		{
			name: "API 410 on delete succeeds and removes finalizer",
			route: &gatewayv1.HTTPRoute{
				Name:        "already-gone-410",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			deleteBeforeSync:  true,
			apiStatusCode:     http.StatusGone,
			wantErr:           false,
			wantFinalizerKept: false,
		},
		{
			name: "API 404 on create still fails",
			route: &gatewayv1.HTTPRoute{
				Name:        "missing-endpoint-create",
				Namespace:   "default",
				Finalizers:  []string{finalizerName},
				Annotations: map[string]string{triggerAnnotation: "true"},
			},
			apiStatusCode:     http.StatusNotFound,
			wantErr:           true,
			wantFinalizerKept: true,
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
				err := env.client.Get(ctx, req.NamespacedName, &current)
				if !tc.wantFinalizerKept {
					if !apierrors.IsNotFound(err) && controllerutil.ContainsFinalizer(&current, finalizerName) {
						t.Fatalf("expected route to be deleted or finalizer removed, err=%v, finalizers=%v", err, current.Finalizers)
					}
					return
				}

				if err != nil {
					t.Fatalf("failed to get route after reconcile: %v", err)
				}
				if !controllerutil.ContainsFinalizer(&current, finalizerName) {
					t.Fatalf("expected finalizer %q to remain, got: %v", finalizerName, current.Finalizers)
				}
				if tc.wantErr && tc.route.Annotations[checksumAnnotation] == "" && current.Annotations[checksumAnnotation] != "" {
					t.Fatalf("expected %q not to be stamped on failed API call, got %q", checksumAnnotation, current.Annotations[checksumAnnotation])
				}
				if tc.wantErr && tc.route.Annotations[checksumAnnotation] != "" && current.Annotations[checksumAnnotation] != tc.route.Annotations[checksumAnnotation] {
					t.Fatalf("expected %q to be preserved on failed unregister, got %q", checksumAnnotation, current.Annotations[checksumAnnotation])
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

func TestRouteEventFilter(t *testing.T) {
	pred := routeEventFilter()
	now := metav1.Now()

	untracked := &gatewayv1.HTTPRoute{
		Name:       "untracked",
		Generation: 1,
	}
	untrackedUpdated := untracked.DeepCopy()
	untrackedUpdated.Generation = 2

	tracked := &gatewayv1.HTTPRoute{
		Name:        "tracked",
		Generation:  1,
		Annotations: map[string]string{triggerAnnotation: "true"},
	}

	trackedStatusOnly := tracked.DeepCopy()
	trackedStatusOnly.Status.Parents = []gatewayv1.RouteParentStatus{
		{ParentRef: gatewayv1.ParentReference{Name: "gw"}},
	}

	trackedDeleting := tracked.DeepCopy()
	trackedDeleting.DeletionTimestamp = &now

	trackedGenBumped := tracked.DeepCopy()
	trackedGenBumped.Generation = 2

	trackedAnnotationRemoved := tracked.DeepCopy()
	delete(trackedAnnotationRemoved.Annotations, triggerAnnotation)

	trackedWithFinalizer := tracked.DeepCopy()
	trackedWithFinalizer.Finalizers = []string{finalizerName}

	if pred.Create(event.CreateEvent{Object: nil}) {
		t.Error("expected nil CreateEvent to be filtered out")
	}
	if pred.Create(event.CreateEvent{Object: untracked}) {
		t.Error("expected untracked CreateEvent to be filtered out")
	}
	if !pred.Create(event.CreateEvent{Object: tracked}) {
		t.Error("expected tracked CreateEvent to pass")
	}
	if !pred.Delete(event.DeleteEvent{Object: tracked}) {
		t.Error("expected DeleteEvent to be skipped")
	}
	if !pred.Generic(event.GenericEvent{Object: tracked}) {
		t.Error("expected tracked GenericEvent to pass")
	}

	updateTests := []struct {
		name   string
		oldObj *gatewayv1.HTTPRoute
		newObj *gatewayv1.HTTPRoute
		want   bool
	}{
		{
			name:   "ignores untracked route changes",
			oldObj: untracked,
			newObj: untrackedUpdated,
			want:   false,
		},
		{
			name:   "ignores status-only update on tracked route",
			oldObj: tracked,
			newObj: trackedStatusOnly,
			want:   false,
		},
		{
			name:   "passes when deletion timestamp is set",
			oldObj: tracked,
			newObj: trackedDeleting,
			want:   true,
		},
		{
			name:   "passes when generation increments",
			oldObj: tracked,
			newObj: trackedGenBumped,
			want:   true,
		},
		{
			name:   "passes when trigger annotation is removed",
			oldObj: tracked,
			newObj: trackedAnnotationRemoved,
			want:   true,
		},
		{
			name:   "passes when finalizer is added",
			oldObj: tracked,
			newObj: trackedWithFinalizer,
			want:   true,
		},
	}

	for _, tc := range updateTests {
		t.Run(tc.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{
				ObjectOld: tc.oldObj,
				ObjectNew: tc.newObj,
			})
			if got != tc.want {
				t.Errorf("Update() = %v, want %v", got, tc.want)
			}
		})
	}
}
