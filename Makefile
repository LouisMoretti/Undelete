.PHONY: build vet fmt tidy check up down logs

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

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f bot
