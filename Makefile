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
	sh scripts/test/classroom-routine-test.sh

# The Mac-side Classroom routine (docs/classroom-setup.md).
CLASSROOM_LABEL := com.felipe-cds.things-hub.classroom
CLASSROOM_INTERVAL ?= 3600
CLASSROOM_PLIST := $(HOME)/Library/LaunchAgents/$(CLASSROOM_LABEL).plist

# Builds the read-only Classroom helper into bin/.
.PHONY: classroom-helper
classroom-helper:
	go build -o bin/thub-classroom ./cmd/thub-classroom

# Logs in to Google once in the browser and stores the refresh token in .env.
.PHONY: classroom-login
classroom-login: classroom-helper
	bin/thub-classroom login

# Runs the routine once now, in the foreground.
.PHONY: classroom-run
classroom-run: classroom-helper
	scripts/classroom-routine.sh

# Installs the launchd job: every CLASSROOM_INTERVAL seconds while the Mac is awake.
.PHONY: classroom-install
classroom-install: classroom-helper
	@case "$(CLASSROOM_INTERVAL)" in ''|*[!0-9]*) echo "CLASSROOM_INTERVAL must be a number of seconds"; exit 1;; esac
	@[ "$(CLASSROOM_INTERVAL)" -ge 300 ] || { echo "CLASSROOM_INTERVAL must be at least 300 seconds"; exit 1; }
	mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs/things-hub
	sed -e 's|@LABEL@|$(CLASSROOM_LABEL)|g' -e 's|@REPO@|$(CURDIR)|g' -e 's|@HOME@|$(HOME)|g' \
		-e 's|@INTERVAL@|$(CLASSROOM_INTERVAL)|g' launchd/things-hub-classroom.plist.in > $(CLASSROOM_PLIST)
	plutil -lint $(CLASSROOM_PLIST)
	launchctl bootout gui/$$(id -u)/$(CLASSROOM_LABEL) 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(CLASSROOM_PLIST)

# Stops and removes the launchd job.
.PHONY: classroom-uninstall
classroom-uninstall:
	launchctl bootout gui/$$(id -u)/$(CLASSROOM_LABEL) 2>/dev/null || true
	rm -f $(CLASSROOM_PLIST)
