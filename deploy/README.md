# Kubernetes

Choose one of `colocated.yaml` and `distributed.yaml`. Both create namespace `parakeet`, a ClusterIP API service and temporary disk volumes. The API keeps its queue in memory; no PVC is created. The colocated variant has two containers in one Pod. In the distributed variant, scale `deployment/parakeet-worker` to add processing capacity.

Create namespace and credentials before applying the chosen manifest. Set the two variables from independently generated secrets:

```sh
kubectl create namespace parakeet
kubectl -n parakeet create secret generic parakeet-keys \
  --from-literal=API_KEY="$API_KEY" \
  --from-literal=WORKER_API_KEY="$WORKER_API_KEY"
kubectl apply -f deploy/colocated.yaml
kubectl -n parakeet rollout status deployment/parakeet-api
kubectl -n parakeet port-forward service/parakeet-api 8080:8080
```

The upload URLs returned during port forwarding are opaque handles. The API resolves its own configured `PUBLIC_BASE_URL`; the SDK does not need to download these URLs. Change that value when introducing an ingress or another stable client-facing address. No ingress or external integration is included.

Both containers run as UID/GID 10001 with a read-only root filesystem, dropped capabilities and independent probes. The API keeps its queue in memory and audio in disk `emptyDir`; worker scratch stays in its own disk `emptyDir`. API upgrades use `Recreate`, so clients and workers never reach two separate queues. Do not scale the API above one replica. API availability during updates is a deliberate limit of this first version.

For the distributed variant:

```sh
kubectl -n parakeet scale deployment/parakeet-worker --replicas=2
```

Each additional worker can consume up to 3.5 CPU and 6 GiB RAM. Reserve resources accordingly. The API requests 50m CPU / 32 MiB and is limited to 500m / 256 MiB; see the README for how retained transcripts add up. Use the smaller limits from `compose.yaml` when testing on a Mac. API process restarts and upgrades lose all queued jobs and results. Clients must resubmit. There is no durable mode, and a PVC does not preserve jobs.

Size the API `data` volume for `MAX_STORAGE_BYTES`. Uploads stream to disk within that quota.

Before a production rollout, pin image digests, choose the public base URL, and add network rules appropriate to the cluster. Allow worker-to-API port 8080. Keep the worker health port private and block `/internal/*` at any external ingress. An API-key holder can access all jobs; this deployment is for a shared internal trust boundary.

The application repository supplies portable examples. Cluster-specific Flux resources, secrets and ingress belong in the infrastructure repository.
