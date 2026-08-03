BIN := bsftp
HELPER := bsftp-preview
HELPER_SRC := helper/bsftp-preview/main.swift

GO ?= go
SWIFTC ?= swiftc

.PHONY: all go helper clean

all: go helper

go:
	$(GO) build -o $(BIN) ./...

helper: $(HELPER)

$(HELPER): $(HELPER_SRC)
	$(SWIFTC) -O -framework AppKit -framework QuickLookUI $(HELPER_SRC) -o $(HELPER)

clean:
	rm -f $(BIN) $(HELPER)
