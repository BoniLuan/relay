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
	docker compose up -d --build
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
