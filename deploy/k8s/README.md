# Deploy with Kubernetes and Argo CD

This Kustomize package deploys the control plane and a single-replica
PostgreSQL instance. Services are `ClusterIP` only: PostgreSQL is not exposed
outside the namespace, and the admin UI is not published through an Ingress
or public load balancer by default.

## Prepare the repository configuration

1. Update `images` in `kustomization.yaml` with the GitHub owner/repository
   containing the published GHCR image. For example:

   ```yaml
   images:
     - name: ghcr.io/OWNER/REPOSITORY
       newName: ghcr.io/acme/opamp-centralized
       newTag: main
   ```

   For controlled rollouts, change `newTag` to the commit SHA tag produced by
   GitHub Actions and commit that change. The Deployment pulls the image when
   restarted with `imagePullPolicy: Always`. If GHCR package visibility is
   private, create an `imagePullSecret` in namespace `opamp` and reference it
   from the Deployment pod spec before syncing.

2. Provision the `opamp-secrets` Secret **out of band** in the target namespace
   configured by `spec.destination.namespace` in the Argo CD Application before
   syncing. The Kustomize manifests intentionally do not set a namespace.
   Do not commit credentials to Git. Use your cluster's approved secret
   manager, External Secrets integration, or a securely managed Secret
   workflow. It must contain these keys:

   | Key | Used by |
   | --- | --- |
   | `DATABASE_URL` | Control plane, for example `postgres://opamp:<URL-escaped-password>@postgres:5432/opamp?sslmode=disable` |
   | `OPAMP_ADMIN_USERNAME` | Operator UI Basic Auth |
   | `OPAMP_ADMIN_PASSWORD` | Operator UI Basic Auth; at least 16 characters |
   | `POSTGRES_PASSWORD` | PostgreSQL initial database password |

   The password in `DATABASE_URL` must match `POSTGRES_PASSWORD`. URL-escape
   special characters in the password. A long random alphanumeric database
   password avoids URL-escaping mistakes. The application runs database
   migrations automatically at startup.

## Install the Argo CD Application

1. Edit `deploy/argocd/application.yaml` and replace
   `https://github.com/OWNER/REPOSITORY.git` with this repository's clone URL.
   Set `targetRevision` to the branch you use for deployment.
2. Set `spec.destination.namespace` to the namespace where you want to deploy.
   Argo CD creates that namespace because `CreateNamespace=true` is enabled.
   Provision the `opamp-secrets` Secret in that namespace before the first
   sync. If provisioning the Secret before Argo CD creates the namespace,
   create the namespace separately first:

   ```sh
   kubectl create namespace <target-namespace>
   ```

3. Ensure Argo CD can read this Git repository and the required Secret exists
   in the configured destination namespace.
4. Apply the Application manifest to the Argo CD namespace:

   ```sh
   kubectl apply -f deploy/argocd/application.yaml
   ```

Argo CD will sync the `deploy/k8s` Kustomize package. Check rollout status:

```sh
kubectl -n <target-namespace> get pods,services,pvc
kubectl -n <target-namespace> rollout status deployment/opamp-control-plane
```

## Agent connectivity and admin access

The in-cluster OpAMP endpoint is:

```text
ws://opamp-control-plane.<target-namespace>.svc.cluster.local:4320/v1/opamp
```

This application build uses plaintext WebSockets; bearer agent tokens and
telemetry are not encrypted in transit. For remote VM agents, expose port 4320
only over a trusted private network, or add and test TLS termination and
`wss://` support before broader deployment.

The admin panel is served on port 4321 and remains cluster-internal. For
temporary access from an operator machine, use a port-forward:

```sh
kubectl -n <target-namespace> port-forward service/opamp-control-plane 4321:4321
```

Then open <http://localhost:4321>. Do not publicly expose the admin service
without TLS and appropriate operator access controls.

## Data and production considerations

PostgreSQL data uses a 10Gi `ReadWriteOnce` PVC; adjust its size and storage
class for the cluster. StatefulSet PVCs persist when pods restart, but this
single-replica PostgreSQL deployment does not provide high availability,
automated backups, or disaster recovery. Configure encrypted backups and a
tested restore procedure before production. Keep the GHCR image tag under
GitOps control and review changes before Argo CD syncs them.
