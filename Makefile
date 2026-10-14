IMAGE := coutito/things-hub
TAG := $(shell git rev-parse --short HEAD)

# Publishes the multi-arch image to Docker Hub.
.PHONY: build
build:
	docker buildx build \
	--push \
	--no-cache \
	--platform linux/amd64,linux/arm64 \
	-t $(IMAGE):$(TAG) .

# Builds the image for this machine only and loads it locally, without pushing.
.PHONY: build-local
build-local:
	docker buildx build --load -t $(IMAGE):$(TAG) .

# Runs the server on 127.0.0.1:8787 with its database in the thub-data volume.
.PHONY: run
run:
	docker run -d --name things-hub --restart unless-stopped --read-only \
		-p 127.0.0.1:8787:8787 \
		-v thub-data:/data \
		$(if $(wildcard .env),--env-file .env) \
		$(IMAGE):$(TAG)

.PHONY: dev
dev:
	docker compose --profile hot up -d

.PHONY: test
test:
	go vet ./...
	go test ./...
	sh skills/things-hub-collect/test/thub-send-test.sh
