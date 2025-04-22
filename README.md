# pod-housekeeper

[![Build Status](https://img.shields.io/github/actions/workflow/status/itspooya/pod-housekeeper/.github%2Fworkflows%2Fbuild-and-push.yml?branch=main)](https://github.com/itspooya/pod-housekeeper/actions/workflows/build-and-push.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/itspooya/pod-housekeeper)](https://goreportcard.com/report/github.com/itspooya/pod-housekeeper)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0) <!-- Or MIT, etc. -->
<!-- Add other badges as needed, e.g., Docker pulls -->

A Kubernetes controller that automatically cleans up old Pods based on their age.

## Motivation

Kubernetes clusters, especially in development or CI/CD environments, can accumulate many completed or failed Pods over time (e.g., from `Job` resources). These leftover Pods consume resources and clutter monitoring dashboards. `pod-housekeeper` provides an automated way to identify and remove these old Pods based on configurable time thresholds.

## Features

*   **Two-Stage Deletion:** Pods are first marked (annotated) after a configurable duration, then deleted after a second configurable duration.
*   **Namespace Exclusion:** Specify namespaces where the housekeeper should not operate.
*   **Owner Limits:** Limit the number of Pods marked for deletion simultaneously per owner (e.g., per `ReplicaSet`, `StatefulSet`, `Job`). Kind-specific limits can also be set.
*   **Annotation Exclusion:** Pods with the `pod-housekeeper/exclude: "true"` annotation are ignored.
*   **Self-Exclusion:** Automatically excludes its own Pod if deployed within the cluster (requires Downward API).
*   **Configurable:** Settings managed via environment variables or a configuration file (using Viper).
*   **Metrics:** Exposes Prometheus metrics for monitoring marked and deleted Pods.

## How it Works

1.  `pod-housekeeper` watches Pods across the cluster (respecting namespace exclusions).
2.  When a Pod's age (`time.Now() - pod.Status.StartTime`) exceeds `POD_HOUSEKEEPER_MARK_DURATION`, the controller checks owner limits and exclusion rules.
3.  If eligible, the Pod is annotated with `pod-housekeeper/marked-for-deletion: <timestamp>`.
4.  The controller continues watching marked Pods.
5.  When the time since the Pod was marked exceeds (`POD_HOUSEKEEPER_DELETE_DURATION - POD_HOUSEKEEPER_MARK_DURATION`), the controller deletes the Pod.

## Configuration

Configuration is managed by Viper, prioritizing:

1.  Command-line flags (currently only `--max-concurrent-reconciles`)
2.  Environment variables (prefixed with `POD_HOUSEKEEPER_`)
3.  Configuration file (specified via `--config` flag)
4.  Default values

| Environment Variable                     | Flag / Config Key             | Default        | Description                                                                                                                               |
| :--------------------------------------- | :---------------------------- | :------------- | :---------------------------------------------------------------------------------------------------------------------------------------- |
| `POD_HOUSEKEEPER_MARK_DURATION`          | `markDuration`                | `24h`          | Duration after which a Pod is eligible to be marked for deletion.                                                                           |
| `POD_HOUSEKEEPER_DELETE_DURATION`        | `deleteDuration`              | `48h`          | Total duration after which a Pod should be deleted. Must be > `markDuration`. Deletion happens `deleteDuration - markDuration` after marking. |
| `POD_HOUSEKEEPER_EXCLUDED_NAMESPACES`    | `excludedNamespaces`          | `""`           | Comma-separated list of namespaces to ignore (e.g., `"kube-system,monitoring"`).                                                          |
| `POD_HOUSEKEEPER_MAX_MARKED_PER_OWNER`   | `maxMarkedPerOwner`           | `1`            | Default maximum number of Pods marked simultaneously per controller owner (e.g., ReplicaSet UID). Minimum 1.                          |
| `POD_HOUSEKEEPER_MAX_CONCURRENT_RECONCILES`| `maxConcurrentReconciles` / `--max-concurrent-reconciles` | `2`            | Maximum number of concurrent reconcile loops. Minimum 1. Flag overrides env/config.                                                       |
| `POD_HOUSEKEEPER_EXCLUDE_SELF`           | `excludeSelf`                 | `true` (if POD_NAME/POD_NAMESPACE env vars are set), `false` otherwise | Whether the controller should ignore its own Pod. Requires Downward API to set POD_NAME and POD_NAMESPACE.                                            |
| `POD_HOUSEKEEPER_CHECK_EXCLUDE_ANNOTATION`| `checkExcludeAnnotation`      | `false`        | If `true`, checks for the `pod-housekeeper/exclude: "true"` annotation on Pods before processing them.                                  |
| `POD_HOUSEKEEPER_MAX_MARKED_PER_OWNER_BY_KIND` | `maxMarkedPerOwnerByKind` | `{}` (empty map) | A map in the config file (not env var) to specify owner limits per Kind (e.g., `ReplicaSet: 5`, `Job: 1`). Uses default if Kind not listed. |

**Example Configuration File (`config.yaml`)**

```yaml
markDuration: 12h
deleteDuration: 24h
excludedNamespaces: "kube-system,logging"
maxMarkedPerOwner: 2
checkExcludeAnnotation: true
excludeSelf: true # Explicitly set if desired
maxMarkedPerOwnerByKind:
  ReplicaSet: 3
  Job: 1
```

Pass this file using the `--config` flag: `/pod-housekeeper --config config.yaml`

## Deployment

### Prerequisites

*   Kubernetes Cluster (>= 1.19 recommended for controller-runtime features)
*   `kubectl` installed

### Building the Docker Image

The included `Dockerfile` builds the controller. The GitHub Actions workflow in `.github/workflows/build-and-push.yml` automatically builds and pushes the image to GHCR (`ghcr.io/itspooya/pod-housekeeper`) on pushes to `main` or version tags (`v*.*.*`).

Build manually:
```bash
docker build -t ghcr.io/itspooya/pod-housekeeper:latest .
# docker push ghcr.io/itspooya/pod-housekeeper:latest
```
*Replace `itspooya` with your GitHub username/organization.*

### Deploying to Kubernetes

Standard Kubernetes manifests are expected to be in the `config/` directory.

1.  **Update Image:** Ensure the `image` field in `config/deployment.yaml` points to your built image (e.g., `ghcr.io/itspooya/pod-housekeeper:latest`).
2.  **Apply Manifests:**  
    You can apply all manifests in the `config/` directory at once:
    ```bash
    kubectl apply -f config/
    ```
    Or individually:
    ```bash
    kubectl apply -f config/namespace.yaml
    kubectl apply -f config/rbac.yaml
    kubectl apply -f config/deployment.yaml
    ```

## Metrics

The controller exposes Prometheus metrics on port `:8080` at the `/metrics` endpoint. Key metrics include:

*   `pod_housekeeper_marked_total{namespace="<namespace>"}`: Counter for Pods marked for deletion.
*   `pod_housekeeper_deleted_total{namespace="<namespace>"}`: Counter for Pods successfully deleted.

## Development

### Prerequisites

*   Go >= 1.24
*   Docker
*   `kubectl` configured for a cluster (or use Kind/Minikube)

### Building

```bash
# Build the binary locally (matching the container name)
go build -o pod-housekeeper ./cmd/manager
```

### Running Tests

```bash
go test -v ./...
```

### Running Locally (Outside Cluster)

You can run the controller locally against a cluster if your `kubectl` is configured.

```bash
# Set required environment variables (adjust values as needed)
export POD_HOUSEKEEPER_MARK_DURATION=1m
export POD_HOUSEKEEPER_DELETE_DURATION=2m
export POD_HOUSEKEEPER_EXCLUDED_NAMESPACES="kube-system"
# export KUBECONFIG=~/.kube/config # Optional: If not default

go run ./cmd/manager/main.go
```

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## License

This project is licensed under the Apache 2.0 License - see the LICENSE file for details. <!-- Or MIT --> 