APP_NAME := gateway
BUILD_DIR ?= bin
GO ?= go
VERSION ?= v0.0.0

GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
CGO_ENABLED ?= 0

LDFLAGS := -s -w -X gateway/internal/buildinfo.Version=$(VERSION)

ifeq ($(GOOS),windows)
EXT := .exe
endif

OUTPUT ?= $(BUILD_DIR)/$(APP_NAME)-$(GOOS)-$(GOARCH)$(EXT)

ifeq ($(OS),Windows_NT)
BUILD_ENV := set "GOOS=$(GOOS)" && set "GOARCH=$(GOARCH)" && set "CGO_ENABLED=$(CGO_ENABLED)" &&
MAKE_DIR := if not exist "$(BUILD_DIR)" mkdir "$(BUILD_DIR)"
REMOVE_DIR := if exist "$(BUILD_DIR)" rmdir /s /q "$(BUILD_DIR)"
else
BUILD_ENV := GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=$(CGO_ENABLED)
MAKE_DIR := mkdir -p "$(BUILD_DIR)"
REMOVE_DIR := rm -rf "$(BUILD_DIR)"
endif

.DEFAULT_GOAL := build
.PHONY: all build windows windows-arm64 linux-amd64 linux-arm64 release clean test help

all: build

build: $(BUILD_DIR)
	@echo Building $(OUTPUT) for $(GOOS)/$(GOARCH)
	$(BUILD_ENV) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(OUTPUT)" .

$(BUILD_DIR):
	@$(MAKE_DIR)

windows:
	@$(MAKE) --no-print-directory build GOOS=windows GOARCH=amd64 OUTPUT="$(BUILD_DIR)/$(APP_NAME).exe"

linux-amd64:
	@$(MAKE) --no-print-directory build GOOS=linux GOARCH=amd64 OUTPUT="$(BUILD_DIR)/$(APP_NAME)-linux-amd64"

linux-arm64:
	@$(MAKE) --no-print-directory build GOOS=linux GOARCH=arm64 OUTPUT="$(BUILD_DIR)/$(APP_NAME)-linux-arm64"

release: windows linux-amd64 linux-arm64

test:
	$(GO) test ./...

clean:
	@$(REMOVE_DIR)

help:
	@echo make build                         Build for the current GOOS and GOARCH.
	@echo make windows                       Build bin/$(APP_NAME).exe for Windows amd64.
	@echo make windows-arm64                 Build a Windows arm64 executable.
	@echo make linux-amd64                   Cross-compile for Linux amd64.
	@echo make linux-arm64                   Cross-compile for Linux arm64.
	@echo make build GOOS=darwin GOARCH=arm64 Cross-compile for any Go target.
	@echo make release VERSION=v1.0.0        Build the standard release artifacts.
