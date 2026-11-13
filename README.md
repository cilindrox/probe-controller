# probe-controller

A lightweight Kubernetes controller built with `controller-runtime` that watches
Gateway API `HTTPRoute` resources and sends webhook notifications to an external
API on **create**, **update**, and **delete** events.

## How It Works

1. **Opt-in via Annotation**: Only `HTTPRoute` resources annotated with
    `probe-controller.local/register: "true"` are synced.
2. **Spec Hashing**: A SHA-256 hash of `route.spec` is kept as a 
    `probe-controller.local/checksum` annotation on the HTTPRoute so status-only updates from Gateway controllers do not trigger duplicate API calls.
3. **Finalizer Cleanup**: Attaches `probe-controller.local/finalizer` to tracked
    routes so `DELETED` events are reliably delivered before Kubernetes removes the resource from etcd.

## Configuration

The following environment variables are supported:

| Variable             | Required | Description                                                               |
| ---                  | ---      | ---                                                                       |
| `EXTERNAL_API_URL`   | No*      | Target HTTP/HTTPS endpoint that receives `POST` JSON payloads.            |
| `EXTERNAL_API_TOKEN` | No       | Optional Bearer token sent in the `Authorization: Bearer <token>` header. |

> **NOTE**: one of `--api-url` flag or `EXTERNAL_API_URL` environment variable
> are required.

These should be mounted from a Secret source on the namespace, e.g.:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: prober-config
  namespace: prober-system
type: Opaque
stringData:
  EXTERNAL_API_TOKEN: 'my-secret-token'
    EXTERNAL_API_URL: 'https://webhooks.example.com'
```

### CLI Flags

The following CLI flags can be passed to the binary:

| Flag                          | Default | Description                                                             |
| ---                           | ---     | ---                                                                     |
| `--api-url`                   | `""`    | External endpoint URL to deliver payloads to.                           |
| `--leader-elect`              | `false` | Enable leader election for high-availability multi-replica deployments. |
| `--metrics-bind-address`      | `:8080` | Address the Prometheus metrics endpoint binds to.                       |
| `--health-probe-bind-address` | `:8081` | Address the `/healthz` and `/readyz` endpoints bind to.                 |

## Usage

Annotate any `HTTPRoute` with `probe-controller.local/register: "true"` to
enable webhook notifications:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: example-route
  namespace: default
  annotations:
    probe-controller.local/register: "true"
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - "api.example.com"
  rules:
    - backendRefs:
        - name: my-service
          port: 8080
```

### Webhook Payload Format

The controller sends an HTTP `POST` with `Content-Type: application/json`:

```json
{
  "event": "CREATED",
  "name": "example-route",
  "namespace": "default",
  "hostnames": ["api.example.com"]
}
```

* `event` is one of `CREATED`, `UPDATED`, or `DELETED`.
* `hostnames` is omitted on `DELETED` events.

## Deployment

Update the image reference and Secret values (`EXTERNAL_API_URL` and
    `EXTERNAL_API_TOKEN`) in `deploy/manifests/deploy.yaml`.

Apply the manifests to your cluster:

```bash
kubectl apply --server-side -f deploy.yaml
```

## Running Locally

```bash
export EXTERNAL_API_URL="https://webhooks.example.com"
go run main.go
```

### Run Tests & Linter

```bash
make test
make lint
```

### Build Multi-Arch Container Image

Build `linux/amd64` and `linux/arm64` container images locally using Docker
Buildx Bake:

```bash
make bake
```
