BIN_DIR := $(HOME)/.local/bin
AGENTS  := $(HOME)/Library/LaunchAgents

.PHONY: build test install launchd-install launchd-uninstall clean

build:
	go build -o bin/power ./cmd/power

test:
	go vet ./... && go test ./...

# Copy then rename: overwriting a binary launchd is executing leaves a
# half-mapped image and the restart dies.
install: build
	@mkdir -p $(BIN_DIR)
	cp bin/power $(BIN_DIR)/power.tmp && mv -f $(BIN_DIR)/power.tmp $(BIN_DIR)/power
	@echo "installed $(BIN_DIR)/power"

# Runs `power serve` at login and restarts it if it dies. launchd does not
# read your shell profile, so PATH is spelled out.
launchd-install: install
	@mkdir -p $(AGENTS) $(HOME)/.kami
	sed -e "s#__HOME__#$(HOME)#g" -e "s#__PATH__#$(BIN_DIR):/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin#g" \
	  deploy/ai.power.plist > $(AGENTS)/ai.power.plist
	-launchctl bootout gui/$$(id -u) $(AGENTS)/ai.power.plist 2>/dev/null
	launchctl bootstrap gui/$$(id -u) $(AGENTS)/ai.power.plist && echo "loaded ai.power; log: $(HOME)/.kami/power.log"

launchd-uninstall:
	-launchctl bootout gui/$$(id -u) $(AGENTS)/ai.power.plist; rm -f $(AGENTS)/ai.power.plist

clean:
	rm -rf bin
