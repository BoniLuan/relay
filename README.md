# Relay

A Go webhook delivery service, starting with a runnable scaffold and optional deployment into the shared Kubernetes learning lab.

**Current status:** HTTP server, JSON startup/shutdown logs, graceful shutdown, `/livez`, `/readyz`, Docker Compose and Kubernetes manifests. Event ingestion, PostgreSQL, signing, authentication and delivery workers are not implemented. Readiness currently means only that this dependency-free HTTP scaffold can serve requests.

On the shared VPS, read `/home/luan/projects/INFRASTRUCTURE.md` before deployment. Relay has no public hostname, production deployment, shared-network connection, or persistent data yet.

## Run the scaffold

From this directory, with Docker:

```bash
make up
curl --fail http://127.0.0.1:18081/
curl --fail http://127.0.0.1:18081/readyz
docker compose logs relay-api
make down
```

With Go 1.27 installed: `make test`, `make vet`, and `make run`. Native execution uses the same loopback port 18081, so stop the Compose version first. `RELAY_HTTP_ADDR` overrides the native bind address.

To test without installing Go on the host:

```bash
docker run --rm --network none --cpus 1 --memory 512m --user "$(id -u):$(id -g)" -e GOCACHE=/tmp/go-cache -v "$PWD:/src:ro" -w /src golang:1.27-alpine go test ./...
```

## Structure

```text
cmd/relay/             process startup, configuration, shutdown
internal/httpapi/      HTTP routes and route tests
deploy/kubernetes/     namespace, API Deployment and private Service
docs/ARCHITECTURE.md   intended delivery design and boundaries
docs/ROADMAP.md        implementation milestones
docs/KUBERNETES.md     deploy Relay into the shared cluster
```

Keep the structure small. Add delivery/domain/storage packages when their first behavior is implemented. The Go module is `github.com/BoniLuan/relay`.

Develop the application using [the roadmap](docs/ROADMAP.md). Study Kubernetes independently in [platform-lab](https://github.com/BoniLuan/platform-lab), and use [Relay’s deployment guide](docs/KUBERNETES.md) when you want to deploy this application. The shared cluster is `portfolio-lab`; Relay owns only the `relay-lab` namespace. Cluster configuration and credentials live in the platform repository.
