test:
	@echo "  >  Running unit tests without race detector"
	go test -timeout 120s ./...

test-race:
	@echo "  >  Running unit tests with race detector"
	go test -short -race -timeout 120s ./...

