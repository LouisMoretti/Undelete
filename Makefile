.PHONY: build vet fmt tidy check test-integration test-restore test-restore-media test-coverage up down logs

# The 4 delivery commands, grouped together.
check:
	cd bot && go mod tidy && go build ./... && go vet ./... && gofmt -l .

tidy:
	cd bot && go mod tidy

build:
	cd bot && go build ./...

vet:
	cd bot && go vet ./...

fmt:
	cd bot && gofmt -l .

test-integration:
	./scripts/test-integration.sh

# Combined unit + integration statement coverage with a floor gate
# (default 75%, override with COVERAGE_FLOOR=...). Boots its own
# disposable Postgres like test-integration; touches nothing existing.
test-coverage:
	./scripts/test-coverage.sh

# Restores a backup into a distinct disposable database and verifies it.
# See docs/backup-restore.md (RPO/RTO, periodic recipe).
test-restore:
	./scripts/restore-test.sh

# Restores the PAIR (database dump + media archives) into disposable
# containers and a disposable directory, then reconciles the restored
# catalogue against the restored tree.
# See docs/backup-restore.md (media RPO/RTO, retention, restore order).
test-restore-media:
	./scripts/restore-media-test.sh

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f bot
