# Installs the deploy tool to ~/bin

BINARY_NAME=deploy
INSTALL_DIR=$(HOME)/bin

.PHONY: all build install clean

all: build

build:
	# Builds all files in the current directory (package main)
	go build -o $(BINARY_NAME) .

install: build
	mkdir -p $(INSTALL_DIR)
	mv $(BINARY_NAME) $(INSTALL_DIR)/$(BINARY_NAME)
	@echo "✅ Installed 'deploy' to $(INSTALL_DIR)"
	@echo "   Ensure $(INSTALL_DIR) is in your PATH."

clean:
	rm -f $(BINARY_NAME)