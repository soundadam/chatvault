test:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test ./...

test-race:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test -race ./...

vet:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) vet ./...

fmt-check:
	@unformatted="$$(gofmt -l cmd internal)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt required on:" >&2; echo "$$unformatted" >&2; exit 1; fi

check: fmt-check vet test
