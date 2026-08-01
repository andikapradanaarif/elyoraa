.PHONY: build test vet run docker-build clean

build:
	go build -o elyoraa .

test:
	go test ./...

vet:
	go vet ./...

run: build
	./elyoraa

docker-build:
	docker build -t elyoraa -f deploy/Dockerfile ..

clean:
	rm -f elyoraa
