.PHONY: build vet fmt tidy check test-integration test-restore up down logs

# Les 4 commandes de livraison, regroupées.
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

# Restaure une sauvegarde dans une base jetable distincte et la vérifie.
# Voir docs/backup-restore.md (RPO/RTO, recette périodique).
test-restore:
	./scripts/restore-test.sh

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f bot
