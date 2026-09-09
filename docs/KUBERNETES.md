> **Scaffold reference only:** the durable-ingestion application now requires
> `RELAY_DATABASE_URL` and an explicitly migrated PostgreSQL database. The manifests
> below do not supply them and will not run the current application as-is. Compose
> is the supported development path. Adapting these optional manifests is a later
> milestone; cluster lifecycle remains in platform-lab.

# Deploy Relay into the shared learning cluster

Relay owns its application manifests in `deploy/kubernetes/`. Cluster creation, tooling and credentials belong to [platform-lab](https://github.com/BoniLuan/platform-lab). Relay also works independently through Go or Compose.

## Prerequisite

Follow the [shared Kubernetes walkthrough](https://github.com/BoniLuan/platform-lab/blob/main/docs/KUBERNETES.md) to install tools and create the **portfolio-lab** cluster. This is a one-time prerequisite for the cluster, not a step for each application.

## Deploy (run from the Relay directory)

```bash
make image
make lab-load
make lab-apply
make lab-status
make lab-forward
```

These commands build `relay:lab`, load it into `portfolio-lab`, create Relay's **relay-lab** namespace, and deploy its API and internal Service. The namespace and cluster deliberately have different names: one cluster can contain namespaces for several applications.

Commands use `../platform-lab/.local/kubeconfig` and explicitly select context `kind-portfolio-lab`. Set `LAB_KUBECONFIG=/absolute/path/to/kubeconfig` if your checkout layout differs. The cluster name/context remains fixed to this learning environment.

While port-forward runs, use another terminal:

```bash
curl --fail http://127.0.0.1:18080/
curl --fail http://127.0.0.1:18080/readyz
```

The application has no public route or persistent data. `18080` is lab access; Compose/native execution uses `18081`. Health currently confirms only the scaffold HTTP server, not a working webhook pipeline.

## Remove only Relay

```bash
make lab-remove
```

This deletes the `relay-lab` namespace and its namespaced resources. Future volume data handling will depend on storage reclaim policies; the scaffold has no persistent storage. It leaves the shared cluster and other application namespaces intact. Use the platform repository only when you intend to delete the entire cluster.

For Pod replacement, troubleshooting and later deployment exercises, follow the [shared walkthrough](https://github.com/BoniLuan/platform-lab/blob/main/docs/KUBERNETES.md).
