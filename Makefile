CLI := bin/telegram-harvest
GO_FILES := $(shell find cmd internal -type f -name '*.go' | sort)
GO_SOURCE_SET := $(shell printf '%s\n' '$(GO_FILES)' | cksum | awk '{print $$1 "-" $$2}')
GO_SOURCE_STAMP := bin/.go-sources-$(GO_SOURCE_SET)
PROFILE ?=
PROFILE_ARG = --profile "$(PROFILE)"
REQUIRE_PROFILE = @test -n "$(strip $(PROFILE))" || { printf "PROFILE=main|study is required\n"; exit 2; }
MEDIA_LIMIT_FLAGS = \
	$(if $(strip $(MAX_PHOTO_BYTES)),--max-photo-bytes "$(MAX_PHOTO_BYTES)",) \
	$(if $(strip $(MAX_DOCUMENT_BYTES)),--max-document-bytes "$(MAX_DOCUMENT_BYTES)",) \
	$(if $(strip $(MAX_AUDIO_BYTES)),--max-audio-bytes "$(MAX_AUDIO_BYTES)",) \
	$(if $(strip $(MAX_VIDEO_BYTES)),--max-video-bytes "$(MAX_VIDEO_BYTES)",)

.DEFAULT_GOAL := help

.PHONY: help setup build fmt fmt-check test race check audit verify doctor login daily daily-catchup daily-download-media transcribe-file chats topics dump sync download-media compact agent-view refresh-agent-view clean

help:
	@printf "Available commands:\\n"
	@printf "  make setup   # download pinned Go dependencies\\n"
	@printf "  make build   # build the reusable local CLI binary\\n"
	@printf "  make fmt     # gofmt project files\\n"
	@printf "  make test    # go test ./...\\n"
	@printf "  make race    # run tests with the race detector\\n"
	@printf "  make check   # formatting, module, vet, and test validation\\n"
	@printf "  make audit   # static analysis and reachable vulnerability scan\\n"
	@printf "  make verify  # full local and CI validation\\n"
	@printf "  make doctor PROFILE=main|study # show config/session status\\n"
	@printf "  make login PROFILE=main|study  # create MTProto user session\\n"
	@printf "  make daily PROFILE=main DATE=today|yesterday|YYYY-MM-DD # build one daily report\\n"
	@printf "  make daily-catchup PROFILE=main # generate missing days and one merged handoff file\\n"
	@printf "  make daily-download-media PROFILE=main # manual uncapped daily media fetch; CHAT=... MESSAGE_ID=...\\n"
	@printf "  make transcribe-file PROFILE=main # adaptive local production ASR; INPUT=... OUTPUT=...\\n"
	@printf "  make chats PROFILE=study # list dialogs; pass QUERY='вшэ' to filter\\n"
	@printf "  make topics PROFILE=study # list topics for CHAT=<allowed forum id>\\n"
	@printf "  make dump PROFILE=study # dump allowed study chat; CHAT=... OUT=...\\n"
	@printf "  make sync PROFILE=study # incremental sync for CHAT=... NAME=...\\n"
	@printf "  make download-media PROFILE=study # manual uncapped media fetch; CHAT=... MESSAGE_ID=...\\n"
	@printf "  make compact PROFILE=study # low-level: compact an existing JSONL for agents\\n"
	@printf "  make agent-view PROFILE=study # low-level: build Markdown navigation from JSONL\\n"
	@printf "  make refresh-agent-view PROFILE=study # update Markdown navigation and messages.toon\\n"
	@printf "  make clean   # remove build artifacts; keep harvested data and runtime state\\n"

setup:
	go mod download

build: $(CLI)

$(GO_SOURCE_STAMP):
	@mkdir -p "$(dir $@)"
	@rm -f bin/.go-sources-*
	@touch "$@"

$(CLI): $(GO_SOURCE_STAMP) $(GO_FILES) go.mod go.sum
	@mkdir -p "$(dir $@)"
	go build -trimpath -o "$@" ./cmd/telegram-harvest

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; test -z "$$unformatted" || { printf "Unformatted Go files:\\n%s\\n" "$$unformatted"; exit 1; }

test:
	go test ./...

race:
	go test -race ./...

check: fmt-check
	go mod tidy -diff
	go mod verify
	go vet ./...
	go test ./...

audit:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...

verify: check race audit

doctor login daily daily-catchup daily-download-media transcribe-file chats topics dump sync download-media compact agent-view: $(CLI)

doctor:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) doctor

login:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) login

daily:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) daily --date "$(or $(DATE),today)" $(if $(strip $(OUT)),--out "$(OUT)",) $(if $(strip $(MARKDOWN_OUT)),--markdown-out "$(MARKDOWN_OUT)",) $(if $(strip $(DIALOG_LIMIT)),--dialog-limit "$(DIALOG_LIMIT)",) $(if $(strip $(DOWNLOAD_MEDIA)),--download-media="$(DOWNLOAD_MEDIA)",) $(if $(strip $(MEDIA_DIR)),--media-dir "$(MEDIA_DIR)",) $(MEDIA_LIMIT_FLAGS) $(if $(strip $(TRANSCRIBE)),--transcribe="$(TRANSCRIBE)",) $(if $(strip $(TRANSCRIBE_VIDEO)),--transcribe-video="$(TRANSCRIBE_VIDEO)",) $(if $(strip $(PROGRESS)),--progress,)

daily-catchup:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) daily-catchup $(if $(strip $(FROM)),--from "$(FROM)",) $(if $(strip $(REPORT_DIR)),--report-dir "$(REPORT_DIR)",) $(if $(strip $(DIALOG_LIMIT)),--dialog-limit "$(DIALOG_LIMIT)",) $(if $(strip $(DOWNLOAD_MEDIA)),--download-media="$(DOWNLOAD_MEDIA)",) $(if $(strip $(MEDIA_DIR)),--media-dir "$(MEDIA_DIR)",) $(MEDIA_LIMIT_FLAGS) $(if $(strip $(TRANSCRIBE)),--transcribe="$(TRANSCRIBE)",) $(if $(strip $(TRANSCRIBE_VIDEO)),--transcribe-video="$(TRANSCRIBE_VIDEO)",) $(if $(strip $(PROGRESS)),--progress,)

daily-download-media:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) daily-download-media --chat "$(CHAT)" --message-id "$(MESSAGE_ID)" $(if $(strip $(INDEX)),--index "$(INDEX)",) $(if $(strip $(OUT_DIR)),--out-dir "$(OUT_DIR)",) $(if $(strip $(OVERWRITE)),--overwrite,) $(if $(strip $(JSON)),--json,)

transcribe-file:
	$(REQUIRE_PROFILE)
	@test -n "$(strip $(INPUT))" || { printf "INPUT is required\\n"; exit 2; }
	@test -n "$(strip $(OUTPUT))" || { printf "OUTPUT is required\\n"; exit 2; }
	$(CLI) $(PROFILE_ARG) transcribe-file --input "$(INPUT)" --output "$(OUTPUT)"

chats:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) chats $(if $(strip $(QUERY)),--query "$(QUERY)",)

topics:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) topics --chat "$(CHAT)"

dump:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) dump --chat "$(CHAT)" --out "$(or $(OUT),chat.jsonl)" $(if $(strip $(FROM)),--from "$(FROM)",) $(if $(strip $(TO)),--to "$(TO)",) $(if $(strip $(ALL)),--all,) $(if $(strip $(LIMIT)),--limit "$(LIMIT)",) $(if $(strip $(DOWNLOAD_MEDIA)),--download-media="$(DOWNLOAD_MEDIA)",) $(if $(strip $(MEDIA_DIR)),--media-dir "$(MEDIA_DIR)",) $(MEDIA_LIMIT_FLAGS) $(if $(strip $(TRANSCRIBE)),--transcribe="$(TRANSCRIBE)",) $(if $(strip $(TRANSCRIBE_VIDEO)),--transcribe-video="$(TRANSCRIBE_VIDEO)",) $(if $(strip $(TRANSCRIPT_DIR)),--transcript-dir "$(TRANSCRIPT_DIR)",) $(if $(strip $(ASR_LOG)),--asr-log "$(ASR_LOG)",)

sync:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) sync --chat "$(CHAT)" --name "$(NAME)" $(if $(strip $(ALL)),--all,) $(if $(strip $(RESET)),--reset,) $(if $(strip $(RESET_MERGED)),--reset-merged,) $(if $(strip $(MERGED_OUT)),--merged-out "$(MERGED_OUT)",) $(if $(strip $(DOWNLOAD_MEDIA)),--download-media="$(DOWNLOAD_MEDIA)",) $(if $(strip $(MEDIA_DIR)),--media-dir "$(MEDIA_DIR)",) $(MEDIA_LIMIT_FLAGS)

download-media:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) download-media --chat "$(CHAT)" --message-id "$(MESSAGE_ID)" $(if $(strip $(INDEX)),--index "$(INDEX)",) $(if $(strip $(OUT_DIR)),--out-dir "$(OUT_DIR)",) $(if $(strip $(OVERWRITE)),--overwrite,) $(if $(strip $(JSON)),--json,)

compact:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) compact --in "$(or $(IN),messages.jsonl)" --out "$(or $(OUT),messages.toon)" $(if $(strip $(SINCE)),--since "$(SINCE)",) $(if $(strip $(LIMIT)),--limit "$(LIMIT)",) $(if $(strip $(INCLUDE_SERVICE)),--include-service,)

agent-view:
	$(REQUIRE_PROFILE)
	$(CLI) $(PROFILE_ARG) agent-view --in "$(or $(IN),messages.jsonl)" --out-dir "$(or $(OUT_DIR),agent-view)" $(if $(strip $(SINCE)),--since "$(SINCE)",) $(if $(strip $(RECENT)),--recent "$(RECENT)",) $(if $(strip $(INCLUDE_SERVICE)),--include-service,) $(if $(strip $(REBUILD)),--rebuild,)

refresh-agent-view: agent-view compact

clean:
	rm -rf artifacts telegram-harvest bin
