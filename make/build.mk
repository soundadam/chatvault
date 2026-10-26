build:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) build -o $(BINARY) ./cmd/chatvault

verify-mod:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) mod verify
