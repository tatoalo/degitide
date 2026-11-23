GO ?= go
PYTHON ?= python3
VENV ?= .venv
BINARY := bin/degitide
SURE_TEST_COMPOSE := docker compose -f tests/integration/sure/compose.yml -f tests/integration/sure/compat.yml

.PHONY: build run worker install-worker install-browser test test-browser test-sure test-sure-down fmt docker-build docker-up docker-native-login docker-sync-mode docker-dashboard-token docker-down clean

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) ./cmd/degitide

run: build
	./$(BINARY)

worker:
	cd worker && ../$(VENV)/bin/python main.py

install-worker:
	$(PYTHON) -m venv $(VENV)
	$(VENV)/bin/pip install -r worker/requirements.txt

install-browser:
	$(VENV)/bin/patchright install chromium

test:
	node --test scripts/tests/portfolio.test.cjs
	$(GO) test ./...
	cd worker && ../$(VENV)/bin/python -m unittest discover -s tests -v
	$(VENV)/bin/python -m unittest discover -s proxy/tests -v

test-browser:
	$(VENV)/bin/python tests/browser/valuation_sync.py
	$(VENV)/bin/python tests/browser/portfolio_valuation.py
	$(VENV)/bin/python tests/browser/schedule_settings.py
	$(VENV)/bin/python tests/browser/sync_progress.py
	$(VENV)/bin/python tests/browser/degiro_onboarding.py
	$(VENV)/bin/python tests/browser/changelog.py
	$(VENV)/bin/python tests/browser/sure_onboarding.py

# Uses only the separate degitide-sure-e2e project and synthetic credentials.
test-sure:
	$(SURE_TEST_COMPOSE) up -d --build --wait --wait-timeout 180
	$(PYTHON) tests/integration/sure/run.py

test-sure-down:
	$(SURE_TEST_COMPOSE) down --volumes --remove-orphans

fmt:
	gofmt -w cmd internal

docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-native-login:
	./scripts/native-login.sh

docker-sync-mode:
	docker compose -f docker-compose.split.yml up --force-recreate --remove-orphans -d

docker-dashboard-token:
	@docker compose exec -T app /bin/sh -c 'cat "$${DEGITIDE_API_TOKEN_FILE}"'

docker-down:
	docker compose down

clean:
	rm -f $(BINARY)
	find worker -type f -name '*.pyc' -delete
	find worker -type d -name '__pycache__' -empty -delete
