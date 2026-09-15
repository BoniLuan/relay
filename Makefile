export RELAY_RUN_UID := $(shell id -u)
export RELAY_RUN_GID := $(shell id -g)

IMAGE ?= relay:lab
LAB_KUBECONFIG ?= $(abspath ../platform-lab/.local/kubeconfig)
KUBECTL = kubectl --kubeconfig "$(LAB_KUBECONFIG)" --context kind-portfolio-lab

.PHONY: test vet run up down image lab-load lab-apply lab-status lab-forward lab-remove

test:
	go test ./...
vet:
	go vet ./...
run:
	go run ./cmd/relay
up:
	docker compose up -d --build relay-api
down:
	docker compose down
image:
	docker build -t "$(IMAGE)" .
lab-load:
	kind load docker-image "$(IMAGE)" --name portfolio-lab
lab-apply:
	$(KUBECTL) apply -f deploy/kubernetes/
	$(KUBECTL) -n relay-lab rollout status deployment/relay-api --timeout=120s
lab-status:
	$(KUBECTL) -n relay-lab get pods,services
lab-forward:
	$(KUBECTL) -n relay-lab port-forward --address 127.0.0.1 service/relay-api 18080:80
lab-remove:
	$(KUBECTL) delete namespace relay-lab

# The test database uses tmpfs and never publishes a host port.
.PHONY: test-integration db migrate client

test-integration:
	@sh scripts/test-integration.sh

db:
	docker compose up -d relay-db
migrate:
	docker compose run --rm --build relay-admin migrate
client:
	docker compose run --rm relay-admin create-client "$(NAME)"

.PHONY: keyring register-keyring
keyring:
	mkdir -p .local
	docker build -t relay:dev .
	docker run --rm --network none --user "$(RELAY_RUN_UID):$(RELAY_RUN_GID)" -v "$(CURDIR)/.local:/keys" relay:dev keyring-init /keys/keyring.json
register-keyring:
	docker compose run --rm relay-admin register-keyring

WORKER_LEASE ?= 30s
.PHONY: worker
worker:
	docker compose run --rm --build relay-worker worker --lease-duration "$(WORKER_LEASE)"

.PHONY: test-worker-process
test-worker-process:
	docker build -t relay:lease-check .
	sh scripts/test-worker-process.sh

.PHONY: deliver-once
deliver-once:
	docker compose run --rm --build relay-delivery-worker worker --send

.PHONY: worker-start worker-stop worker-logs
worker-start:
	docker compose up -d --build relay-delivery-worker
worker-stop:
	docker compose stop relay-delivery-worker
worker-logs:
	docker compose logs --tail 100 -f relay-delivery-worker

.PHONY: metrics
metrics:
	@docker compose build relay-admin 1>&2
	@docker compose run --rm --no-deps -T relay-admin metrics
