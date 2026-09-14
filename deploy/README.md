# Kubernetes

Choose one of `colocated.yaml` and `distributed.yaml`. Both create namespace `parakeet`, a ClusterIP API service and temporary disk volumes. SQLite runs in memory; no PVC is created. The colocated variant has two containers in one Pod. In the distributed variant, scale `deployment/parakeet-worker` to add processing capacity.

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

Both containers run as UID/GID 10001 with a read-only root filesystem, dropped capabilities and independent probes. The API keeps SQLite in memory and audio in disk `emptyDir`; worker scratch stays in its own disk `emptyDir`. API upgrades use `Recreate` to avoid concurrent ownership of SQLite. Do not scale the API above one replica. API availability during updates is a deliberate limit of this first version.

For the distributed variant:

```sh
kubectl -n parakeet scale deployment/parakeet-worker --replicas=2
```

Each additional worker can consume up to 3.5 CPU and 6 GiB RAM. Reserve resources accordingly. Use the smaller limits from `compose.yaml` when testing on a Mac. API process restarts lose jobs and results. For durable jobs, explicitly set `DB_MODE=file` and replace the API `data` volume with a PVC:

```yaml
# In the parakeet-api Deployment:
        env:
        - name: DB_MODE
          value: file
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: parakeet-api-data
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: parakeet-api-data
  namespace: parakeet
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 3Gi
```

Use block storage, not NFS. Size the volume for `MAX_STORAGE_BYTES`, multipart spooling (`MAX_CONCURRENT_UPLOADS × MAX_UPLOAD_BYTES`) and SQLite with its WAL. Keep `strategy: Recreate`, so two API Pods never mount the volume at once.

Before a production rollout, pin image digests, choose the public base URL, and add network rules appropriate to the cluster. Allow worker-to-API port 8080. Keep the worker health port private and block `/internal/*` at any external ingress. An API-key holder can access all jobs; this deployment is for a shared internal trust boundary.

The application repository supplies portable examples. Cluster-specific Flux resources, secrets and ingress belong in the infrastructure repository.
