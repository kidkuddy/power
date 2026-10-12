BIN_DIR := $(HOME)/.local/bin
AGENTS  := $(HOME)/Library/LaunchAgents

.PHONY: build test install launchd-install launchd-uninstall clean

build:
	go build -o bin/kami-slack ./cmd/kami-slack

test:
	go vet ./... && go test ./...

# Copy then rename: overwriting a binary launchd is executing leaves a
# half-mapped image and the restart dies.
install: build
	@mkdir -p $(BIN_DIR)
	cp bin/kami-slack $(BIN_DIR)/kami-slack.tmp && mv -f $(BIN_DIR)/kami-slack.tmp $(BIN_DIR)/kami-slack
	@echo "installed $(BIN_DIR)/kami-slack"

# Runs `kami-slack serve` at login and restarts it if it dies. launchd does not
# read your shell profile, so PATH is spelled out.
launchd-install: install
	@mkdir -p $(AGENTS) $(HOME)/.kami
	sed -e "s#__HOME__#$(HOME)#g" -e "s#__PATH__#$(BIN_DIR):/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin#g" \
	  deploy/ai.kami.slack.plist > $(AGENTS)/ai.kami.slack.plist
	-launchctl bootout gui/$$(id -u) $(AGENTS)/ai.kami.slack.plist 2>/dev/null
	launchctl bootstrap gui/$$(id -u) $(AGENTS)/ai.kami.slack.plist && echo "loaded ai.kami.slack; log: $(HOME)/.kami/kami-slack.log"

launchd-uninstall:
	-launchctl bootout gui/$$(id -u) $(AGENTS)/ai.kami.slack.plist; rm -f $(AGENTS)/ai.kami.slack.plist

clean:
	rm -rf bin
