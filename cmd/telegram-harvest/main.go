package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/chupakobra6/telegram-harvest/internal/config"
	"github.com/chupakobra6/telegram-harvest/internal/harvest"
	"github.com/chupakobra6/telegram-harvest/internal/mtproto"
	"github.com/chupakobra6/telegram-harvest/internal/runlock"
	"github.com/chupakobra6/telegram-harvest/internal/stages"
	"github.com/chupakobra6/telegram-harvest/internal/transcribe"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin, stdout, stderr *os.File) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	profile, args, err := extractProfileArg(args)
	if err != nil {
		return printError(stderr, 2, err)
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printUsage(stdout)
		return 0
	}
	command := args[0]
	if !knownCommand(command) {
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
	if strings.TrimSpace(profile) == "" {
		return printError(stderr, 2, fmt.Errorf("--profile main|study is required"))
	}
	projectRoot := detectProjectRoot()
	if err := loadToolDotEnv(projectRoot); err != nil {
		return printError(stderr, 1, err)
	}
	cfg, err := loadProfileConfig(profile)
	if err != nil {
		return printError(stderr, 1, err)
	}
	cfg = cfg.WithRoot(projectRoot)
	includeDailyRuntime := cfg.Mode == config.ModeMain
	if command == "transcribe-file" {
		if cfg.Mode != config.ModeMain {
			return printError(stderr, 1, fmt.Errorf("transcribe-file is supported only for profile main"))
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := runTranscribeFile(ctx, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	}
	client := mtproto.New(cfg)

	switch command {
	case "print-config":
		printConfig(cfg, stdout, includeDailyRuntime)
		return 0
	case "doctor":
		printDoctor(cfg, stdout, client, includeDailyRuntime)
		return 0
	case "login":
		if err := withRuntimeLock(cfg, func() error {
			return client.Login(context.Background(), stdin, stdout)
		}); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "send-saved":
		if err := runSendSaved(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "daily":
		if err := runDaily(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "daily-catchup":
		if err := runDailyCatchup(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "daily-download-media":
		if err := runDownloadMedia(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "me":
		if err := runMe(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "chats":
		if err := runChats(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "topics":
		if err := runTopics(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "dump":
		if err := runDump(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "download-media":
		if err := runDownloadMedia(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "sync":
		if err := runSync(cfg, client, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "compact":
		if err := runCompact(cfg, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	case "agent-view":
		if err := runAgentView(cfg, args[1:], stdout); err != nil {
			return printError(stderr, 1, err)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func knownCommand(command string) bool {
	switch command {
	case "print-config", "doctor", "login", "daily", "daily-catchup", "daily-download-media",
		"me", "chats", "topics", "dump", "download-media", "sync", "compact", "agent-view", "transcribe-file", "send-saved":
		return true
	default:
		return false
	}
}

func extractProfileArg(args []string) (string, []string, error) {
	result := make([]string, 0, len(args))
	profile := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--profile" || arg == "-profile":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a value", arg)
			}
			if profile != "" {
				return "", nil, fmt.Errorf("--profile specified more than once")
			}
			profile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--profile="):
			if profile != "" {
				return "", nil, fmt.Errorf("--profile specified more than once")
			}
			profile = strings.TrimPrefix(arg, "--profile=")
		case strings.HasPrefix(arg, "-profile="):
			if profile != "" {
				return "", nil, fmt.Errorf("--profile specified more than once")
			}
			profile = strings.TrimPrefix(arg, "-profile=")
		default:
			result = append(result, arg)
		}
	}
	return profile, result, nil
}

func loadProfileConfig(profile string) (config.Config, error) {
	return config.LoadProfile(profile)
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: telegram-harvest --profile main|study <doctor|print-config|login|me|chats|topics|dump|sync|download-media|compact|agent-view|daily|daily-catchup|daily-download-media|transcribe-file|send-saved> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Harvesting operations are read-only; send-saved is the only Telegram write operation")
	fmt.Fprintln(out, "  --profile main|study  # required account profile")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Primary daily workflow:")
	fmt.Fprintln(out, "  daily --date today [--markdown-out reports/daily/YYYY-MM-DD.md] [--download-media=false] [--transcribe-video phone|all|off]")
	fmt.Fprintln(out, "  daily-catchup [--from YYYY-MM-DD] [--report-dir reports/daily] [--download-media=false] [--transcribe-video phone|all|off]")
	fmt.Fprintln(out, "  daily-download-media --chat <id-or-username> --message-id 123 --index 1 [--out-dir media-manual]")
	fmt.Fprintln(out, "  transcribe-file --input recording.mp4 --output transcript.txt  # adaptive local production ASR; profile main only")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Account and discovery:")
	fmt.Fprintln(out, "  me [--json]")
	fmt.Fprintln(out, "  chats --query вшэ --limit 300 [--json]  # output is filtered by the study allowlist when set")
	fmt.Fprintln(out, "  topics --chat <allowed-id-or-username> --limit 200 [--json]")
	fmt.Fprintln(out, "  send-saved --text <message> [--json]  # @Pheik13 main session -> InputPeerSelf only")
	fmt.Fprintln(out, "  send-saved --file </absolute/path> [--caption <message>] [--json]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Low-level agent primitives:")
	fmt.Fprintln(out, "  Relative input/output paths below are resolved inside the selected profile state directory")
	fmt.Fprintln(out, "  dump --chat <allowed-id-or-username> --from 2026-06-11 --to 2026-06-22 --all --out hse-main.jsonl [--download-media --media-dir media] [--transcribe --asr-log asr.jsonl]")
	fmt.Fprintln(out, "  sync --chat <allowed-id-or-username> --name hse-main [--all --reset] [--merged-out messages.jsonl] [--download-media --media-dir media]")
	fmt.Fprintln(out, "  download-media --chat <allowed-id-or-username> --message-id 123 --index 1 [--out-dir media-manual]")
	fmt.Fprintln(out, "  compact --in messages.jsonl --out messages.toon [--since 2026-05-01] [--limit 500]")
	fmt.Fprintln(out, "  agent-view --in messages.jsonl --out-dir agent-view [--recent 300] [--rebuild]")
}

func printError(stderr io.Writer, code int, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(stderr, err)
	return code
}

func printConfig(cfg config.Config, out io.Writer, includeDailyRuntime bool) {
	fmt.Fprintf(out, "profile=%s\n", config.ProfileName(cfg.Mode))
	fmt.Fprintf(out, "app_id_set=%t\n", cfg.AppID != 0)
	fmt.Fprintf(out, "app_hash_set=%t\n", strings.TrimSpace(cfg.AppHash) != "")
	fmt.Fprintf(out, "phone_set=%t\n", strings.TrimSpace(cfg.Phone) != "")
	fmt.Fprintf(out, "session=%s\n", cfg.SessionPath)
	fmt.Fprintf(out, "runtime_lock=%s\n", cfg.RuntimeLockPath())
	fmt.Fprintf(out, "state_dir=%s\n", cfg.StateDir)
	fmt.Fprintf(out, "allowed_chats=%d\n", cfg.AllowedChatCount())
	fmt.Fprintf(out, "daily_additional_senders=%d\n", cfg.DailyAdditionalSenderCount())
	fmt.Fprintf(out, "rpc_spacing=%s\n", cfg.RPCSpacing)
	if includeDailyRuntime {
		printDailyRuntimeConfig(out, false)
	}
}

func printDoctor(cfg config.Config, out io.Writer, client *mtproto.Client, includeDailyRuntime bool) {
	fmt.Fprintf(out, "profile=%s\n", config.ProfileName(cfg.Mode))
	fmt.Fprintf(out, "app_id_set=%t\n", cfg.AppID != 0)
	fmt.Fprintf(out, "app_hash_set=%t\n", strings.TrimSpace(cfg.AppHash) != "")
	fmt.Fprintf(out, "phone_set=%t\n", strings.TrimSpace(cfg.Phone) != "")
	fmt.Fprintf(out, "session_path=%s\n", cfg.SessionPath)
	fmt.Fprintf(out, "session_exists=%t\n", fileExists(cfg.SessionPath))
	fmt.Fprintf(out, "runtime_lock_path=%s\n", cfg.RuntimeLockPath())
	fmt.Fprintf(out, "state_dir=%s\n", cfg.StateDir)
	fmt.Fprintf(out, "state_dir_exists=%t\n", fileExists(cfg.StateDir))
	fmt.Fprintf(out, "allowed_chats=%d\n", cfg.AllowedChatCount())
	fmt.Fprintf(out, "daily_additional_senders=%d\n", cfg.DailyAdditionalSenderCount())
	fmt.Fprintf(out, "harvesting_read_only=true\n")
	fmt.Fprintf(out, "saved_messages_send_enabled=%t\n", cfg.Mode == config.ModeMain)
	if includeDailyRuntime {
		printDailyRuntimeConfig(out, true)
	}
	authStatus, authDetail := doctorAuthStatus(cfg, client)
	fmt.Fprintf(out, "auth_status=%s\n", authStatus)
	if authDetail != "" {
		fmt.Fprintf(out, "auth_status_detail=%s\n", authDetail)
	}
}

func printDailyRuntimeConfig(out io.Writer, includeChecks bool) {
	defaults := dailyRuntimeDefaults()
	fmt.Fprintf(out, "daily_transcribe_default=%t\n", defaults.TranscribeMedia)
	fmt.Fprintf(out, "daily_asr_backend=%s\n", transcribe.BackendWhisperCPP)
	fmt.Fprintf(out, "daily_whisper_command=%s\n", defaults.WhisperCommand)
	fmt.Fprintf(out, "daily_whisper_model_path=%s\n", defaults.WhisperModelPath)
	fmt.Fprintf(out, "daily_whisper_accelerator=%s\n", transcribe.AcceleratorMetal)
	fmt.Fprintf(out, "daily_whisper_speech_gate_model_path=%s\n", defaults.WhisperGateFilePath)
	fmt.Fprintf(out, "daily_asr_language=%s\n", transcribe.ProductionLanguage)
	fmt.Fprintf(out, "daily_asr_workers=1\n")
	fmt.Fprintf(out, "daily_ffmpeg_command=%s\n", defaults.FFmpegCommand)
	if !includeChecks {
		return
	}
	if resolved, ok := resolveCommand(defaults.FFmpegCommand); ok {
		fmt.Fprintf(out, "daily_ffmpeg_status=ok:%s\n", resolved)
	} else {
		fmt.Fprintf(out, "daily_ffmpeg_status=missing\n")
	}
	if resolved, ok := resolveCommand(defaults.WhisperCommand); ok {
		fmt.Fprintf(out, "daily_whisper_command_status=ok:%s\n", resolved)
	} else {
		fmt.Fprintf(out, "daily_whisper_command_status=missing\n")
	}
	if fileExists(defaults.WhisperModelPath) {
		fmt.Fprintf(out, "daily_whisper_model_status=ok\n")
	} else {
		fmt.Fprintf(out, "daily_whisper_model_status=missing\n")
	}
	if fileExists(defaults.WhisperGateFilePath) {
		fmt.Fprintf(out, "daily_whisper_speech_gate_status=ok\n")
	} else {
		fmt.Fprintf(out, "daily_whisper_speech_gate_status=missing\n")
	}
}

func doctorAuthStatus(cfg config.Config, client *mtproto.Client) (string, string) {
	if cfg.AppID == 0 || strings.TrimSpace(cfg.AppHash) == "" {
		return "skipped", fmt.Sprintf("set %s and %s to verify live Telegram authorization", cfg.EnvNames("APP_ID"), cfg.EnvNames("APP_HASH"))
	}
	if !fileExists(cfg.SessionPath) {
		return "reauth_required", fmt.Sprintf("session file is missing; run `%s`", cfg.LoginCommand())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := client.AuthStatus(ctx)
	if err != nil {
		return "check_failed", oneLine(err.Error())
	}
	if status.Authorized {
		return "authorized", "Telegram accepted the current session"
	}
	return "reauth_required", fmt.Sprintf("Telegram requires re-login; run `%s`", cfg.LoginCommand())
}

func runChats(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("chats", flag.ContinueOnError)
	fs.SetOutput(out)
	limit := fs.Int("limit", 300, "maximum dialogs to scan")
	query := fs.String("query", "", "case-insensitive title/username/id filter")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withRuntimeLock(cfg, func() error {
		return client.RunAuthorized(context.Background(), func(ctx context.Context, session *mtproto.Session) error {
			chats, err := session.ListDialogs(ctx, *limit, *query)
			if err != nil {
				return err
			}
			chats = filterAllowedChats(cfg, chats)
			if *jsonOut {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(chats)
			}
			for _, chat := range chats {
				fmt.Fprintf(out, "%d\t%s\t%s", chat.ID, chat.Type, chat.Display)
				if chat.Username != "" {
					fmt.Fprintf(out, "\t@%s", chat.Username)
				}
				if !chat.LastMessageAt.IsZero() {
					fmt.Fprintf(out, "\tlast=%s", chat.LastMessageAt.Format(time.RFC3339))
				}
				if chat.UnreadCount > 0 {
					fmt.Fprintf(out, "\tunread=%d", chat.UnreadCount)
				}
				fmt.Fprintln(out)
			}
			return nil
		})
	})
}

func runTopics(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("topics", flag.ContinueOnError)
	fs.SetOutput(out)
	chat := fs.String("chat", "", "forum chat id or @username")
	limit := fs.Int("limit", 200, "maximum topics to list")
	query := fs.String("query", "", "optional topic title search")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*chat) == "" {
		return fmt.Errorf("--chat is required")
	}
	if err := ensureAllowedChat(cfg, *chat); err != nil {
		return err
	}
	return withRuntimeLock(cfg, func() error {
		return client.RunAuthorized(context.Background(), func(ctx context.Context, session *mtproto.Session) error {
			topics, err := session.ListTopics(ctx, *chat, *limit, *query)
			if err != nil {
				return err
			}
			if *jsonOut {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(topics)
			}
			for _, topic := range topics {
				fmt.Fprintf(out, "%d\t%s", topic.ID, topic.Title)
				if topic.TopMessageID > 0 {
					fmt.Fprintf(out, "\ttop_message=%d", topic.TopMessageID)
				}
				if !topic.LastMessageAt.IsZero() {
					fmt.Fprintf(out, "\tlast=%s", topic.LastMessageAt.Format(time.RFC3339))
				}
				if topic.UnreadCount > 0 {
					fmt.Fprintf(out, "\tunread=%d", topic.UnreadCount)
				}
				if topic.Pinned {
					fmt.Fprint(out, "\tpinned")
				}
				if topic.Closed {
					fmt.Fprint(out, "\tclosed")
				}
				if topic.Hidden {
					fmt.Fprint(out, "\thidden")
				}
				fmt.Fprintln(out)
			}
			return nil
		})
	})
}

func runMe(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("me", flag.ContinueOnError)
	fs.SetOutput(out)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withRuntimeLock(cfg, func() error {
		return client.RunAuthorized(context.Background(), func(ctx context.Context, session *mtproto.Session) error {
			profile, err := session.SelfProfile(ctx)
			if err != nil {
				return err
			}
			if *jsonOut {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(profile)
			}
			fmt.Fprintf(out, "id=%d\n", profile.ID)
			if profile.Username != "" {
				fmt.Fprintf(out, "username=@%s\n", profile.Username)
			}
			if profile.Display != "" {
				fmt.Fprintf(out, "display=%s\n", profile.Display)
			}
			if profile.Phone != "" {
				fmt.Fprintf(out, "phone=%s\n", maskCLIPhone(profile.Phone))
			}
			return nil
		})
	})
}

func runSendSaved(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("send-saved", flag.ContinueOnError)
	fs.SetOutput(out)
	textValue := fs.String("text", "", "text message to send to Saved Messages")
	filePath := fs.String("file", "", "file to send to Saved Messages")
	caption := fs.String("caption", "", "optional file caption")
	jsonOut := fs.Bool("json", false, "print verified readback as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("send-saved does not accept positional arguments or a recipient")
	}

	var result mtproto.SendSavedResult
	err := withRuntimeLock(cfg, func() error {
		var sendErr error
		result, sendErr = client.SendSaved(context.Background(), mtproto.SendSavedOptions{
			Text:     *textValue,
			FilePath: *filePath,
			Caption:  *caption,
		})
		return sendErr
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}

	fmt.Fprintf(out, "sent_to=saved_messages\n")
	fmt.Fprintf(out, "account=@%s\n", result.Profile.Username)
	fmt.Fprintf(out, "message_id=%d\n", result.Message.MessageID)
	fmt.Fprintf(out, "verified=%t\n", result.Verified)
	if len(result.Message.Attachments) == 1 {
		attachment := result.Message.Attachments[0]
		fmt.Fprintf(out, "file_name=%s\n", attachment.FileName)
		fmt.Fprintf(out, "mime_type=%s\n", attachment.MIMEType)
		fmt.Fprintf(out, "byte_size=%d\n", attachment.Size)
	}
	return nil
}

func maskCLIPhone(phone string) string {
	trimmed := strings.TrimSpace(phone)
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "+") {
		trimmed = "+" + trimmed
	}
	if len(trimmed) <= 5 {
		return trimmed
	}
	return trimmed[:2] + strings.Repeat("*", len(trimmed)-4) + trimmed[len(trimmed)-2:]
}

func runDump(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	fs.SetOutput(out)
	defaults := dailyRuntimeDefaults()
	chat := fs.String("chat", "", "chat id or @username")
	output := fs.String("out", "", "JSONL output path, relative to state dir unless absolute")
	limit := fs.Int("limit", config.DefaultHistoryLimit, "maximum records")
	sinceID := fs.Int("since-id", 0, "only export messages with id greater than this")
	fromRaw := fs.String("from", "", "inclusive lower date bound, YYYY-MM-DD or RFC3339")
	toRaw := fs.String("to", "", "exclusive upper date bound, YYYY-MM-DD or RFC3339")
	all := fs.Bool("all", false, "export all available history")
	topicID := fs.Int("topic", 0, "forum topic id to export via replies")
	topicTitle := fs.String("topic-title", "", "optional topic title stored in output metadata")
	downloadMedia := fs.Bool("download-media", false, "download supported photo/document attachments while exporting")
	mediaDir := fs.String("media-dir", "media", "media output directory, relative to state dir unless absolute")
	transcribeMedia := fs.Bool("transcribe", false, "transcribe voice/audio/video media; cached transcripts skip media download")
	transcribeVideo := fs.String("transcribe-video", harvest.VideoTranscribePhone, "generic video transcription mode: phone, all, or off")
	whisperCommand := fs.String("whisper-command", defaults.WhisperCommand, "whisper.cpp Metal server command")
	whisperModelPath := fs.String("whisper-model", defaults.WhisperModelPath, "whisper.cpp large-v3-turbo q5_0 model file")
	whisperGateFilePath := fs.String("whisper-speech-gate-model", defaults.WhisperGateFilePath, "Silero model for the whole-file speech gate")
	ffmpegCommand := fs.String("ffmpeg-command", defaults.FFmpegCommand, "ffmpeg command for audio extraction and WAV conversion")
	transcriptDir := fs.String("transcript-dir", "transcripts", "transcript output directory, relative to state dir unless absolute")
	asrLogPath := fs.String("asr-log", "", "optional ASR event JSONL output path, relative to state dir unless absolute")
	mediaLimits := addMediaLimitFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*chat) == "" {
		return fmt.Errorf("--chat is required")
	}
	if err := ensureAllowedChat(cfg, *chat); err != nil {
		return err
	}
	if strings.TrimSpace(*output) == "" {
		return fmt.Errorf("--out is required")
	}
	start, end, err := parseHistoryBounds(*fromRaw, *toRaw)
	if err != nil {
		return err
	}
	if err := validateDailyOptions(dailyOptions{VideoTranscribeMode: *transcribeVideo}); err != nil {
		return err
	}
	if *transcribeMedia && cfg.Mode != config.ModeMain {
		return fmt.Errorf("dump --transcribe is supported only for profile main")
	}
	outputPath := resolveOutputPath(cfg.StateDir, *output)
	history := harvest.HistoryOptions{
		Limit:               *limit,
		BatchSize:           config.DefaultBatchSize,
		MinID:               *sinceID,
		Start:               start,
		End:                 end,
		All:                 *all,
		TopicID:             *topicID,
		TopicTitle:          *topicTitle,
		TranscribeMedia:     *transcribeMedia,
		VideoTranscribeMode: *transcribeVideo,
		WhisperCommand:      *whisperCommand,
		WhisperModelPath:    *whisperModelPath,
		WhisperGateFilePath: *whisperGateFilePath,
		FFmpegCommand:       *ffmpegCommand,
	}
	if *downloadMedia {
		history.DownloadMedia = true
		history.MediaDir = resolveOutputPath(cfg.StateDir, *mediaDir)
		applyMediaLimits(&history, mediaLimits)
		history.ManualDownloadCommand = "telegram-harvest download-media"
	}
	if *transcribeMedia {
		history.TranscriptDir = resolveOutputPath(cfg.StateDir, *transcriptDir)
	}
	if *all {
		history.Limit = 0
		history.MaxBatches = 0
	}
	if *transcribeMedia {
		transcribeOpts := dailyTranscribeOptions(history)
		if !transcribeOpts.Configured() {
			return fmt.Errorf("--transcribe requires a configured transcriber")
		}
	}
	var asrLogFile *os.File
	if strings.TrimSpace(*asrLogPath) != "" {
		asrEncoder, file, err := harvest.OpenJSONL(resolveOutputPath(cfg.StateDir, *asrLogPath), false)
		if err != nil {
			return err
		}
		asrLogFile = file
		history.ASRLog = func(event harvest.ASRLogEvent) error {
			return asrEncoder.Encode(event)
		}
	}
	runErr := withRuntimeLock(cfg, func() error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return client.RunAuthorized(ctx, func(ctx context.Context, session *mtproto.Session) error {
			encoder, file, err := harvest.OpenJSONL(outputPath, false)
			if err != nil {
				return err
			}
			defer file.Close()
			_, stats, err := session.DumpHistory(ctx, *chat, history, func(record harvest.MessageRecord) error {
				return encoder.Encode(record)
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "wrote=%d out=%s first_id=%d last_id=%d batches=%d flood_waits=%d\n",
				stats.Records,
				outputPath,
				stats.FirstID,
				stats.LastID,
				stats.Batches,
				stats.FloodWaits,
			)
			return nil
		})
	})
	if asrLogFile != nil {
		if closeErr := asrLogFile.Close(); runErr == nil && closeErr != nil {
			runErr = closeErr
		}
	}
	return runErr
}

func runSync(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(out)
	chat := fs.String("chat", "", "chat id or @username")
	name := fs.String("name", "", "local stream name")
	limit := fs.Int("limit", config.DefaultHistoryLimit, "maximum new records")
	mergedOut := fs.String("merged-out", "", "optional append-only merged JSONL output, relative to state dir unless absolute")
	all := fs.Bool("all", false, "sync all available history")
	reset := fs.Bool("reset", false, "truncate this stream and reset its state before syncing")
	resetMerged := fs.Bool("reset-merged", false, "truncate merged output before writing")
	topicID := fs.Int("topic", 0, "forum topic id to sync via replies")
	topicTitle := fs.String("topic-title", "", "optional topic title stored in state metadata")
	downloadMedia := fs.Bool("download-media", false, "download supported photo/document attachments while syncing")
	mediaDir := fs.String("media-dir", "media", "media output directory, relative to state dir unless absolute")
	mediaLimits := addMediaLimitFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*chat) == "" {
		return fmt.Errorf("--chat is required")
	}
	if err := ensureAllowedChat(cfg, *chat); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("--name is required")
	}
	streamPath := filepath.Join(cfg.StateDir, *name+".jsonl")
	statePath := filepath.Join(cfg.StateDir, *name+".state.json")
	mergedPath := ""
	if strings.TrimSpace(*mergedOut) != "" {
		mergedPath = resolveOutputPath(cfg.StateDir, *mergedOut)
	}
	history := harvest.HistoryOptions{
		Limit:      *limit,
		BatchSize:  config.DefaultBatchSize,
		All:        *all,
		TopicID:    *topicID,
		TopicTitle: *topicTitle,
	}
	if *downloadMedia {
		history.DownloadMedia = true
		history.MediaDir = resolveOutputPath(cfg.StateDir, *mediaDir)
		applyMediaLimits(&history, mediaLimits)
		history.ManualDownloadCommand = "telegram-harvest download-media"
	}
	if *all {
		history.Limit = 0
		history.MaxBatches = 0
	}
	return withRuntimeLock(cfg, func() error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return client.RunAuthorized(ctx, func(ctx context.Context, session *mtproto.Session) error {
			result, err := harvest.RunSync(ctx, session, harvest.SyncOptions{
				Chat:        *chat,
				StreamPath:  streamPath,
				StatePath:   statePath,
				MergedPath:  mergedPath,
				History:     history,
				Reset:       *reset,
				ResetMerged: *resetMerged,
				Progress: func(progress harvest.SyncProgress) {
					printSyncProgress(out, progress)
				},
			})
			if err != nil {
				if errors.Is(err, context.Canceled) {
					printInterruptedSync(out, statePath)
					return nil
				}
				return err
			}
			mode := "incremental"
			if *all {
				mode = "all"
			}
			if *all && result.State.Backfill != nil {
				fmt.Fprintf(out, "mode=%s complete=%t synced=%d total_records=%d stream=%s state=%s last_id=%d next_offset_id=%d batches=%d flood_waits=%d\n",
					mode,
					result.State.Backfill.Complete,
					result.Stats.Records,
					result.State.Backfill.Records,
					result.StreamPath,
					result.StatePath,
					result.State.LastID,
					result.State.Backfill.NextOffsetID,
					result.State.Backfill.Batches,
					result.Stats.FloodWaits,
				)
				return nil
			}
			fmt.Fprintf(out, "mode=%s synced=%d stream=%s state=%s last_id=%d batches=%d flood_waits=%d\n",
				mode,
				result.Stats.Records,
				result.StreamPath,
				result.StatePath,
				result.State.LastID,
				result.Stats.Batches,
				result.Stats.FloodWaits,
			)
			return nil
		})
	})
}

func runDaily(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	dateDefault := "today"
	dateLabelDefault, _, _, _ := parseDailyDate(dateDefault, time.Now())
	defaultJSONL, defaultMarkdown := harvest.DailyDefaultOutputPaths(cfg.StateDir, dateLabelDefault)
	defaults := dailyRuntimeDefaults()

	fs := flag.NewFlagSet("daily", flag.ContinueOnError)
	fs.SetOutput(out)
	dateRaw := fs.String("date", dateDefault, "day to harvest: today, yesterday, or YYYY-MM-DD in Europe/Moscow")
	output := fs.String("out", defaultJSONL, "JSONL output path; relative paths are resolved under state dir")
	markdownOut := fs.String("markdown-out", defaultMarkdown, "Markdown report output path; default writes to visible reports/daily")
	dailyFlags := addDailyOptionFlags(fs, defaults)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dateLabel, start, end, err := parseDailyDate(*dateRaw, time.Now())
	if err != nil {
		return err
	}
	if !flagWasSet(fs, "out") {
		jsonl, _ := harvest.DailyDefaultOutputPaths(cfg.StateDir, dateLabel)
		*output = jsonl
	}
	if !flagWasSet(fs, "markdown-out") {
		_, markdown := harvest.DailyDefaultOutputPaths(cfg.StateDir, dateLabel)
		*markdownOut = markdown
	}
	outputPath := resolveOutputPath(cfg.StateDir, *output)
	markdownPath := ""
	if strings.TrimSpace(*markdownOut) != "" {
		markdownPath = resolveOutputPath(cfg.StateDir, *markdownOut)
	}
	job := dailyJob{
		Date:         dateLabel,
		Start:        start,
		End:          end,
		OutputPath:   outputPath,
		MarkdownPath: markdownPath,
		ASRLogPath:   harvest.DailyDefaultASRLogPath(cfg.StateDir, dateLabel),
	}
	timings := newDailyStageTimingCollector("daily", dateLabel, dateLabel)
	runErr := runDailyJobs(cfg, client, []dailyJob{job}, dailyFlags.values(), timings, out)
	return finishDailyStageTimings(cfg.StateDir, timings, runErr, out)
}

func runDailyCatchup(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	defaults := dailyRuntimeDefaults()
	defaultReportDir := harvest.DailyDefaultReportRoot(cfg.StateDir)

	fs := flag.NewFlagSet("daily-catchup", flag.ContinueOnError)
	fs.SetOutput(out)
	fromRaw := fs.String("from", "", "first day to generate, YYYY-MM-DD; default starts after newest Markdown report")
	reportDirRaw := fs.String("report-dir", defaultReportDir, "directory with daily Markdown reports")
	dailyFlags := addDailyOptionFlags(fs, defaults)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dailyOpts := dailyFlags.values()
	if err := validateDailyOptions(dailyOpts); err != nil {
		return err
	}

	reportDir := resolveReportDirPath(*reportDirRaw)
	plan, err := buildDailyCatchupPlan(cfg, reportDir, *fromRaw, time.Now())
	if err != nil {
		return err
	}
	catchupDates := dailyCatchupDates(plan)
	startDate, endDate := dailyDateBounds(catchupDates)
	timings := newDailyStageTimingCollector("daily-catchup", startDate, endDate)
	if len(plan.Skipped) > 0 {
		for _, skipped := range plan.Skipped {
			fmt.Fprintf(out, "skip date=%s reason=markdown_exists\n", skipped)
		}
	}
	if len(plan.Jobs) == 0 {
		mergedPath := ""
		if len(catchupDates) > 0 {
			renderStart := time.Now()
			mergedPath, err = publishDailyCatchupMarkdown(reportDir, catchupDates)
			stages.ObserveSince(timings.Observe, stages.Render, renderStart)
			if err != nil {
				return finishDailyStageTimings(cfg.StateDir, timings, err, out)
			}
		}
		fmt.Fprintf(out, "catchup up_to_date=true last_report=%s today=%s report_dir=%s skipped=%d",
			plan.LastReport,
			plan.Today,
			reportDir,
			len(plan.Skipped),
		)
		if mergedPath != "" {
			fmt.Fprintf(out, " merged=%s", mergedPath)
		}
		fmt.Fprintln(out)
		return finishDailyStageTimings(cfg.StateDir, timings, nil, out)
	}
	fmt.Fprintf(out, "catchup start=%s end=%s today=%s report_dir=%s planned=%d skipped=%d\n",
		plan.Jobs[0].Date,
		plan.Jobs[len(plan.Jobs)-1].Date,
		plan.Today,
		reportDir,
		len(plan.Jobs),
		len(plan.Skipped),
	)
	checkpointPath := harvest.DailyDialogCheckpointPath(cfg.StateDir)
	previousCheckpoint, checkpointLoadErr := harvest.LoadDailyDialogCheckpoint(checkpointPath)
	checkpointRequest := &dailyCheckpointRequest{
		Previous:         previousCheckpoint,
		LoadErr:          checkpointLoadErr,
		ScopeFingerprint: dailyDialogCheckpointScopeFingerprint(cfg, dailyOpts),
		StartDate:        plan.Jobs[0].Date,
		VerifiedThrough:  plan.Jobs[len(plan.Jobs)-1].Date,
		ForceFull:        strings.TrimSpace(*fromRaw) != "",
	}
	dailyResult, err := runDailyJobsWithCheckpoint(cfg, client, plan.Jobs, dailyOpts, timings, checkpointRequest, out)
	if err != nil {
		return finishDailyStageTimings(cfg.StateDir, timings, err, out)
	}
	renderStart := time.Now()
	mergedPath, err := publishDailyCatchupCompletion(reportDir, catchupDates, checkpointPath, dailyResult.Checkpoint)
	stages.ObserveSince(timings.Observe, stages.Render, renderStart)
	if err != nil {
		return finishDailyStageTimings(cfg.StateDir, timings, err, out)
	}
	fmt.Fprintf(out, "catchup complete=true generated=%d skipped=%d merged=%s checkpoint=%s\n", len(plan.Jobs), len(plan.Skipped), mergedPath, checkpointPath)
	return finishDailyStageTimings(cfg.StateDir, timings, nil, out)
}

type dailyOptionFlags struct {
	dialogLimit         *int
	limit               *int
	includeService      *bool
	downloadMedia       *bool
	mediaDir            *string
	mediaLimits         mediaLimitFlags
	transcribeMedia     *bool
	transcribeVideo     *string
	whisperCommand      *string
	whisperModelPath    *string
	whisperGateFilePath *string
	ffmpegCommand       *string
	transcriptDir       *string
	progress            *bool
}

func addDailyOptionFlags(fs *flag.FlagSet, defaults dailyRuntimeConfig) dailyOptionFlags {
	flags := dailyOptionFlags{
		dialogLimit:         fs.Int("dialog-limit", dailyDialogLimitDefault(), "maximum dialogs to scan"),
		limit:               fs.Int("limit", 0, "maximum newest records to write after filtering; 0 means all"),
		includeService:      fs.Bool("include-service", false, "include Telegram service messages"),
		downloadMedia:       fs.Bool("download-media", true, "download photos and image documents; audio/video is downloaded temporarily for transcription"),
		mediaDir:            fs.String("media-dir", "media", "media output directory, relative to state dir unless absolute"),
		transcribeMedia:     fs.Bool("transcribe", defaults.TranscribeMedia, "transcribe voice/audio/video media; cached transcripts skip media download"),
		transcribeVideo:     fs.String("transcribe-video", harvest.VideoTranscribePhone, "generic video transcription mode: phone, all, or off"),
		whisperCommand:      fs.String("whisper-command", defaults.WhisperCommand, "whisper.cpp Metal server command"),
		whisperModelPath:    fs.String("whisper-model", defaults.WhisperModelPath, "whisper.cpp large-v3-turbo q5_0 model file"),
		whisperGateFilePath: fs.String("whisper-speech-gate-model", defaults.WhisperGateFilePath, "required Silero model for the production whole-file speech gate"),
		ffmpegCommand:       fs.String("ffmpeg-command", defaults.FFmpegCommand, "ffmpeg command for audio extraction and WAV conversion"),
		transcriptDir:       fs.String("transcript-dir", "transcripts", "transcript output directory, relative to state dir unless absolute"),
		progress:            fs.Bool("progress", false, "print per-dialog progress"),
	}
	flags.mediaLimits = addMediaLimitFlags(fs)
	return flags
}

func (f dailyOptionFlags) values() dailyOptions {
	return dailyOptions{
		DialogLimit:          *f.dialogLimit,
		Limit:                *f.limit,
		IncludeService:       *f.includeService,
		DownloadMedia:        *f.downloadMedia,
		MediaDir:             *f.mediaDir,
		MaxPhotoBytes:        *f.mediaLimits.photo,
		MaxDocumentBytes:     *f.mediaLimits.document,
		MaxAudioBytes:        *f.mediaLimits.audio,
		MaxVideoBytes:        *f.mediaLimits.video,
		MaxGenericVideoBytes: harvest.DefaultMaxGenericVideoBytes,
		TranscribeMedia:      *f.transcribeMedia,
		VideoTranscribeMode:  *f.transcribeVideo,
		WhisperCommand:       *f.whisperCommand,
		WhisperModelPath:     *f.whisperModelPath,
		WhisperGateFilePath:  *f.whisperGateFilePath,
		FFmpegCommand:        *f.ffmpegCommand,
		TranscriptDir:        *f.transcriptDir,
		Progress:             *f.progress,
	}
}

type dailyOptions struct {
	DialogLimit          int
	Limit                int
	IncludeService       bool
	DownloadMedia        bool
	MediaDir             string
	MaxPhotoBytes        int64
	MaxDocumentBytes     int64
	MaxAudioBytes        int64
	MaxVideoBytes        int64
	MaxGenericVideoBytes int64
	TranscribeMedia      bool
	VideoTranscribeMode  string
	WhisperCommand       string
	WhisperModelPath     string
	WhisperGateFilePath  string
	FFmpegCommand        string
	TranscriptDir        string
	Progress             bool
}

type dailyJob struct {
	Date         string
	Start        time.Time
	End          time.Time
	OutputPath   string
	MarkdownPath string
	ASRLogPath   string
}

type dailyCatchupPlan struct {
	Jobs       []dailyJob
	Skipped    []string
	LastReport string
	Today      string
}

func dailyCatchupDates(plan dailyCatchupPlan) []string {
	dates := make([]string, 0, len(plan.Jobs)+len(plan.Skipped))
	dates = append(dates, plan.Skipped...)
	for _, job := range plan.Jobs {
		dates = append(dates, job.Date)
	}
	sort.Strings(dates)
	return dates
}

func dailyDateBounds(dates []string) (string, string) {
	if len(dates) == 0 {
		return "", ""
	}
	return dates[0], dates[len(dates)-1]
}

func publishDailyCatchupMarkdown(reportDir string, dates []string) (string, error) {
	outputPath := filepath.Join(reportDir, harvest.DailyLatestCatchupFilename)
	tempPath, err := createAtomicTextPath(outputPath)
	if err != nil {
		return "", err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tempPath)
		}
	}()
	if err := harvest.WriteDailyCatchupMarkdown(harvest.DailyCatchupMarkdownOptions{
		OutputPath: tempPath,
		ReportDir:  reportDir,
		Dates:      dates,
	}); err != nil {
		return "", err
	}
	if err := publishAtomicOutput(tempPath, outputPath); err != nil {
		return "", err
	}
	published = true
	return outputPath, nil
}

func publishDailyCatchupCompletion(
	reportDir string,
	dates []string,
	checkpointPath string,
	checkpoint *harvest.DailyDialogCheckpoint,
) (string, error) {
	mergedPath, err := publishDailyCatchupMarkdown(reportDir, dates)
	if err != nil {
		return "", err
	}
	if checkpoint != nil {
		if err := harvest.SaveDailyDialogCheckpoint(checkpointPath, *checkpoint); err != nil {
			return mergedPath, err
		}
	}
	return mergedPath, nil
}

func runDailyJobs(cfg config.Config, client *mtproto.Client, jobs []dailyJob, opts dailyOptions, timings *dailyStageTimingCollector, out io.Writer) error {
	_, err := runDailyJobsWithCheckpoint(cfg, client, jobs, opts, timings, nil, out)
	return err
}

type dailyCheckpointRequest struct {
	Previous         harvest.DailyDialogCheckpoint
	LoadErr          error
	ScopeFingerprint string
	StartDate        string
	VerifiedThrough  string
	ForceFull        bool
}

type dailyRunResult struct {
	Stats      harvest.OutgoingStats
	Checkpoint *harvest.DailyDialogCheckpoint
}

func runDailyJobsWithCheckpoint(
	cfg config.Config,
	client *mtproto.Client,
	jobs []dailyJob,
	opts dailyOptions,
	timings *dailyStageTimingCollector,
	checkpointRequest *dailyCheckpointRequest,
	out io.Writer,
) (dailyRunResult, error) {
	if len(jobs) == 0 {
		return dailyRunResult{}, nil
	}
	if err := validateDailyOptions(opts); err != nil {
		return dailyRunResult{}, err
	}
	history := dailyHistoryOptions(cfg, opts)
	if timings != nil {
		history.StageTiming = timings.Observe
		history.DownloadTiming = timings.ObserveDownloadTransfer
		history.DownloadQueueTiming = timings.ObserveDownloadQueueWait
		history.AudioDurationTiming = timings.ObserveAudioDuration
		history.MediaPipelineTiming = timings.ObserveMediaPipeline
	}
	additionalSenderIDsByChat := dailyAdditionalSenderIDsByChat(cfg)
	var result dailyRunResult
	err := withRuntimeLock(cfg, func() error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return client.RunAuthorized(ctx, func(ctx context.Context, session *mtproto.Session) error {
			accountID := int64(0)
			if checkpointRequest != nil {
				accountStart := time.Now()
				profile, err := session.SelfProfile(ctx)
				stages.ObserveSince(history.StageTiming, stages.TelegramScan, accountStart)
				if err != nil {
					return fmt.Errorf("resolve checkpoint account: %w", err)
				}
				accountID = profile.ID
				history.DialogCheckpoint = evaluateDailyCheckpointRequest(*checkpointRequest, accountID)
				fmt.Fprintf(out, "dialog_checkpoint enabled=%t fallback=%s\n",
					history.DialogCheckpoint.Enabled,
					history.DialogCheckpoint.FallbackReason,
				)
			}
			rangeStats, err := runDailyRangeJobs(ctx, session, history, opts, jobs, additionalSenderIDsByChat, out)
			result.Stats = rangeStats
			if timings != nil {
				timings.ObserveOutgoingStats(rangeStats)
				if checkpointRequest != nil {
					timings.ObserveDialogCheckpoint(history.DialogCheckpoint, rangeStats)
				}
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					fmt.Fprintf(out, "interrupted=true start=%s end=%s\n", jobs[0].Date, jobs[len(jobs)-1].Date)
				}
				return err
			}
			if checkpointRequest != nil && rangeStats.Complete {
				checkpoint := harvest.NewDailyDialogCheckpoint(
					accountID,
					checkpointRequest.ScopeFingerprint,
					checkpointRequest.VerifiedThrough,
					rangeStats.DialogHeads,
					time.Now(),
				)
				result.Checkpoint = &checkpoint
			}
			return nil
		})
	})
	return result, err
}

func evaluateDailyCheckpointRequest(request dailyCheckpointRequest, accountID int64) harvest.DailyDialogCheckpointDecision {
	if request.ForceFull {
		return harvest.DailyDialogCheckpointDecision{FallbackReason: "explicit_from"}
	}
	return harvest.EvaluateDailyDialogCheckpoint(
		request.Previous,
		request.LoadErr,
		accountID,
		request.ScopeFingerprint,
		request.StartDate,
	)
}

func validateDailyOptions(opts dailyOptions) error {
	switch strings.TrimSpace(opts.VideoTranscribeMode) {
	case "", harvest.VideoTranscribePhone, harvest.VideoTranscribeAll, harvest.VideoTranscribeOff:
	default:
		return fmt.Errorf("--transcribe-video must be one of: %s, %s, %s", harvest.VideoTranscribePhone, harvest.VideoTranscribeAll, harvest.VideoTranscribeOff)
	}
	if opts.TranscribeMedia {
		transcribeOpts := transcribe.ProductionOptions(
			opts.WhisperCommand,
			opts.WhisperModelPath,
			opts.WhisperGateFilePath,
			opts.FFmpegCommand,
			nil,
		)
		if err := transcribeOpts.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func dailyHistoryOptions(cfg config.Config, opts dailyOptions) harvest.HistoryOptions {
	history := harvest.HistoryOptions{
		Limit:                opts.Limit,
		BatchSize:            config.DefaultBatchSize,
		MaxBatches:           0,
		DownloadMedia:        opts.DownloadMedia,
		TranscribeMedia:      opts.TranscribeMedia,
		WhisperCommand:       opts.WhisperCommand,
		WhisperModelPath:     opts.WhisperModelPath,
		WhisperGateFilePath:  opts.WhisperGateFilePath,
		FFmpegCommand:        opts.FFmpegCommand,
		MaxPhotoBytes:        opts.MaxPhotoBytes,
		MaxDocumentBytes:     opts.MaxDocumentBytes,
		MaxAudioBytes:        opts.MaxAudioBytes,
		MaxVideoBytes:        opts.MaxVideoBytes,
		MaxGenericVideoBytes: opts.MaxGenericVideoBytes,
		VideoTranscribeMode:  opts.VideoTranscribeMode,
	}
	history.ManualDownloadCommand = "telegram-harvest daily-download-media"
	if opts.DownloadMedia {
		history.MediaDir = resolveOutputPath(cfg.StateDir, opts.MediaDir)
	}
	if opts.TranscribeMedia {
		history.TranscriptDir = resolveOutputPath(cfg.StateDir, opts.TranscriptDir)
	}
	return history
}

type dailyRangeDumper interface {
	DumpOutgoingRange(context.Context, harvest.OutgoingRangeOptions, func(harvest.MessageRecord) error) (harvest.OutgoingStats, error)
}

type dailyASRLogs struct {
	paths    map[string]string
	encoders map[string]*json.Encoder
	files    []*os.File
	firstErr error
}

func runDailyRangeJobs(
	ctx context.Context,
	dumper dailyRangeDumper,
	history harvest.HistoryOptions,
	opts dailyOptions,
	jobs []dailyJob,
	additionalSenderIDsByChat map[int64][]int64,
	out io.Writer,
) (harvest.OutgoingStats, error) {
	if len(jobs) == 0 {
		return harvest.OutgoingStats{}, nil
	}
	jobs = sortedDailyJobs(jobs)
	asrLogs := newDailyASRLogs(jobs)
	defer func() { _ = asrLogs.Close() }()

	rangeHistory := history
	rangeHistory.Limit = 0
	if len(asrLogs.paths) > 0 {
		rangeHistory.ASRLog = func(event harvest.ASRLogEvent) error {
			job, ok := dailyJobAt(jobs, event.Date)
			if !ok {
				return nil
			}
			return asrLogs.Encode(job.Date, event)
		}
	}
	progress := dailyRangeProgress(out, opts.Progress, jobs[0].Date, jobs[len(jobs)-1].Date)
	records := make([]harvest.MessageRecord, 0)
	rangeStats, err := dumper.DumpOutgoingRange(ctx, harvest.OutgoingRangeOptions{
		Start:                     jobs[0].Start,
		End:                       jobs[len(jobs)-1].End,
		DialogLimit:               opts.DialogLimit,
		IncludeService:            opts.IncludeService,
		AdditionalSenderIDsByChat: additionalSenderIDsByChat,
		IncludeRecord: func(record harvest.MessageRecord) bool {
			_, ok := dailyJobAt(jobs, record.Date)
			return ok
		},
		History:  rangeHistory,
		Progress: progress,
	}, func(record harvest.MessageRecord) error {
		records = append(records, record)
		return nil
	})
	asrErr := asrLogs.Err()
	if err != nil {
		return rangeStats, errors.Join(err, asrErr, asrLogs.Close())
	}
	if asrErr != nil {
		return rangeStats, errors.Join(asrErr, asrLogs.Close())
	}
	if rangeStats.Complete {
		if err := asrLogs.EnsureAll(); err != nil {
			return rangeStats, errors.Join(err, asrLogs.Close())
		}
	}
	if err := asrLogs.Close(); err != nil {
		return rangeStats, err
	}
	if !rangeStats.Complete {
		for _, dialogErr := range rangeStats.DialogErrors {
			fmt.Fprintf(out, "warning dialog_error=%s\n", dialogErr)
		}
		fmt.Fprintf(out, "range start=%s end=%s wrote=%d dialogs=%d attachments=%d transcripts=%d flood_waits=%d complete=false published=false\n",
			jobs[0].Date,
			jobs[len(jobs)-1].Date,
			rangeStats.Records,
			rangeStats.DialogsScanned,
			rangeStats.Attachments,
			rangeStats.Transcripts,
			rangeStats.FloodWaits,
		)
		return rangeStats, fmt.Errorf("daily range %s..%s incomplete; final reports were not published", jobs[0].Date, jobs[len(jobs)-1].Date)
	}

	recordsByDate := partitionDailyRecords(jobs, records, opts.Limit)
	publishedRecords := 0
	for _, job := range jobs {
		dayRecords := recordsByDate[job.Date]
		publishedRecords += len(dayRecords)
		stats := dailyStatsFromRange(rangeStats, dayRecords)
		renderStart := time.Now()
		err := publishDailyJob(job, stats, dayRecords)
		stages.ObserveSince(history.StageTiming, stages.Render, renderStart)
		if err != nil {
			return rangeStats, err
		}
		writeDailyJobResult(out, job, stats)
	}
	fmt.Fprintf(out, "range start=%s end=%s collected=%d published=%d dialogs=%d history_dialogs=%d unchanged=%d changed=%d new=%d batches=%d history_data_pages=%d history_empty_proof_pages=%d history_sparse_continuations=%d checkpoint_proof_candidates=%d checkpoint_proof_stops=%d flood_waits=%d complete=true\n",
		jobs[0].Date,
		jobs[len(jobs)-1].Date,
		len(records),
		publishedRecords,
		rangeStats.DialogsScanned,
		rangeStats.DialogsHistoryRPC,
		rangeStats.DialogsUnchanged,
		rangeStats.DialogsChanged,
		rangeStats.DialogsNew,
		rangeStats.Batches,
		rangeStats.HistoryDataPages,
		rangeStats.HistoryEmptyProofPages,
		rangeStats.HistorySparseContinuations,
		rangeStats.CheckpointProofCandidates,
		rangeStats.CheckpointProofStops,
		rangeStats.FloodWaits,
	)
	return rangeStats, nil
}

func dailyRangeProgress(out io.Writer, enabled bool, start string, end string) func(harvest.OutgoingProgress) error {
	return func(progress harvest.OutgoingProgress) error {
		if !enabled {
			return nil
		}
		if progress.Skipped {
			fmt.Fprintf(out, "progress range=%s..%s skipped=true reason=%s chat=%s total=%d flood_waits=%d\n", start, end, progress.SkipReason, progress.Chat.Display, progress.Total, progress.FloodWaits)
			return nil
		}
		if progress.Error != "" {
			fmt.Fprintf(out, "progress range=%s..%s error=true chat=%s detail=%s total=%d batches=%d flood_waits=%d\n", start, end, progress.Chat.Display, progress.Error, progress.Total, progress.Batches, progress.FloodWaits)
			return nil
		}
		fmt.Fprintf(out, "progress range=%s..%s chat=%s records=%d total=%d batches=%d flood_waits=%d\n", start, end, progress.Chat.Display, progress.Records, progress.Total, progress.Batches, progress.FloodWaits)
		return nil
	}
}

func newDailyASRLogs(jobs []dailyJob) *dailyASRLogs {
	logs := &dailyASRLogs{
		paths:    map[string]string{},
		encoders: map[string]*json.Encoder{},
	}
	for _, job := range jobs {
		if strings.TrimSpace(job.ASRLogPath) == "" {
			continue
		}
		logs.paths[job.Date] = job.ASRLogPath
	}
	return logs
}

func (l *dailyASRLogs) Encode(date string, event harvest.ASRLogEvent) error {
	if l == nil {
		return nil
	}
	encoder, err := l.encoder(date)
	if err != nil || encoder == nil {
		l.noteError(err)
		return err
	}
	err = encoder.Encode(event)
	l.noteError(err)
	return err
}

func (l *dailyASRLogs) EnsureAll() error {
	if l == nil {
		return nil
	}
	for date := range l.paths {
		if _, err := l.encoder(date); err != nil {
			return err
		}
	}
	return nil
}

func (l *dailyASRLogs) encoder(date string) (*json.Encoder, error) {
	if encoder := l.encoders[date]; encoder != nil {
		return encoder, nil
	}
	path := strings.TrimSpace(l.paths[date])
	if path == "" {
		return nil, nil
	}
	encoder, file, err := harvest.OpenJSONL(path, false)
	if err != nil {
		return nil, err
	}
	l.encoders[date] = encoder
	l.files = append(l.files, file)
	return encoder, nil
}

func (l *dailyASRLogs) Err() error {
	if l == nil {
		return nil
	}
	return l.firstErr
}

func (l *dailyASRLogs) noteError(err error) {
	if l != nil && err != nil && l.firstErr == nil {
		l.firstErr = err
	}
}

func (l *dailyASRLogs) Close() error {
	if l == nil {
		return nil
	}
	var firstErr error
	for _, file := range l.files {
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.files = nil
	return firstErr
}

func sortedDailyJobs(jobs []dailyJob) []dailyJob {
	sorted := append([]dailyJob(nil), jobs...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Start.Before(sorted[j].Start)
	})
	return sorted
}

func dailyJobAt(jobs []dailyJob, date time.Time) (dailyJob, bool) {
	index := sort.Search(len(jobs), func(i int) bool {
		return jobs[i].End.After(date)
	})
	if index < len(jobs) && !date.Before(jobs[index].Start) && date.Before(jobs[index].End) {
		return jobs[index], true
	}
	return dailyJob{}, false
}

func partitionDailyRecords(jobs []dailyJob, records []harvest.MessageRecord, limit int) map[string][]harvest.MessageRecord {
	recordsByDate := make(map[string][]harvest.MessageRecord, len(jobs))
	for _, record := range records {
		job, ok := dailyJobAt(jobs, record.Date)
		if !ok {
			continue
		}
		recordsByDate[job.Date] = append(recordsByDate[job.Date], record)
	}
	if limit > 0 {
		for date, dayRecords := range recordsByDate {
			if len(dayRecords) > limit {
				recordsByDate[date] = dayRecords[len(dayRecords)-limit:]
			}
		}
	}
	return recordsByDate
}

func dailyStatsFromRange(rangeStats harvest.OutgoingStats, records []harvest.MessageRecord) harvest.OutgoingStats {
	stats := rangeStats
	stats.Records = len(records)
	stats.DialogsWithRecords = 0
	stats.Attachments = 0
	stats.Transcripts = 0
	stats.Forwarded = 0
	stats.FirstAt = time.Time{}
	stats.LastAt = time.Time{}
	chats := map[int64]struct{}{}
	for _, record := range records {
		chats[record.Chat.ID] = struct{}{}
		if stats.FirstAt.IsZero() || record.Date.Before(stats.FirstAt) {
			stats.FirstAt = record.Date
		}
		if record.Date.After(stats.LastAt) {
			stats.LastAt = record.Date
		}
		if record.Forward != nil {
			stats.Forwarded++
		}
		for _, attachment := range record.Attachments {
			stats.Attachments++
			if strings.TrimSpace(attachment.Transcript) != "" {
				stats.Transcripts++
			}
		}
	}
	stats.DialogsWithRecords = len(chats)
	return stats
}

func publishDailyJob(job dailyJob, stats harvest.OutgoingStats, records []harvest.MessageRecord) error {
	jsonlTempPath, file, err := createAtomicOutput(job.OutputPath)
	if err != nil {
		return err
	}
	publishedJSONL := false
	defer func() {
		_ = file.Close()
		if !publishedJSONL {
			_ = os.Remove(jsonlTempPath)
		}
	}()
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}

	markdownTempPath := ""
	publishedMarkdown := false
	if job.MarkdownPath != "" {
		markdownTempPath, err = createAtomicTextPath(job.MarkdownPath)
		if err != nil {
			return err
		}
		defer func() {
			if !publishedMarkdown {
				_ = os.Remove(markdownTempPath)
			}
		}()
		if err := harvest.WriteDailyMarkdown(harvest.DailyMarkdownOptions{
			OutputPath: markdownTempPath,
			Date:       job.Date,
			Start:      job.Start,
			End:        job.End,
			Stats:      stats,
			Records:    records,
		}); err != nil {
			return err
		}
	}
	if err := publishAtomicOutput(jsonlTempPath, job.OutputPath); err != nil {
		return err
	}
	publishedJSONL = true
	if markdownTempPath != "" {
		if err := publishAtomicOutput(markdownTempPath, job.MarkdownPath); err != nil {
			return err
		}
		publishedMarkdown = true
	}
	return nil
}

func writeDailyJobResult(out io.Writer, job dailyJob, stats harvest.OutgoingStats) {
	fmt.Fprintf(out, "date=%s wrote=%d dialogs=%d dialogs_with_records=%d attachments=%d transcripts=%d out=%s",
		job.Date,
		stats.Records,
		stats.DialogsScanned,
		stats.DialogsWithRecords,
		stats.Attachments,
		stats.Transcripts,
		job.OutputPath,
	)
	if job.MarkdownPath != "" {
		fmt.Fprintf(out, " markdown=%s", job.MarkdownPath)
	}
	if job.ASRLogPath != "" {
		fmt.Fprintf(out, " asr_log=%s", job.ASRLogPath)
	}
	fmt.Fprintf(out, " flood_waits=%d complete=%t\n", stats.FloodWaits, stats.Complete)
}

func createAtomicOutput(path string) (string, *os.File, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil, fmt.Errorf("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", nil, fmt.Errorf("prepare output dir: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp output: %w", err)
	}
	return file.Name(), file, nil
}

func createAtomicTextPath(path string) (string, error) {
	tempPath, file, err := createAtomicOutput(path)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return tempPath, nil
}

func publishAtomicOutput(tempPath string, finalPath string) error {
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return fmt.Errorf("prepare output dir: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("publish %s: %w", finalPath, err)
	}
	return nil
}

type mediaLimitFlags struct {
	photo    *int64
	document *int64
	audio    *int64
	video    *int64
}

func addMediaLimitFlags(fs *flag.FlagSet) mediaLimitFlags {
	return mediaLimitFlags{
		photo:    fs.Int64("max-photo-bytes", harvest.DefaultMaxPhotoBytes, "maximum photo/image bytes to download; 0 disables this cap"),
		document: fs.Int64("max-document-bytes", harvest.DefaultMaxDocumentBytes, "maximum generic document bytes to download; 0 disables this cap"),
		audio:    fs.Int64("max-audio-bytes", harvest.DefaultMaxAudioBytes, "maximum voice/audio bytes to download or transcribe; 0 disables this cap"),
		video:    fs.Int64("max-video-bytes", harvest.DefaultMaxVideoBytes, "maximum video/round-video bytes to download or transcribe; 0 disables this cap"),
	}
}

func applyMediaLimits(opts *harvest.HistoryOptions, limits mediaLimitFlags) {
	if opts == nil {
		return
	}
	opts.MaxPhotoBytes = *limits.photo
	opts.MaxDocumentBytes = *limits.document
	opts.MaxAudioBytes = *limits.audio
	opts.MaxVideoBytes = *limits.video
}

func runDownloadMedia(cfg config.Config, client *mtproto.Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("download-media", flag.ContinueOnError)
	fs.SetOutput(out)
	chat := fs.String("chat", "", "chat id or @username")
	messageID := fs.Int("message-id", 0, "Telegram message id")
	index := fs.Int("index", 1, "1-based attachment index")
	outDir := fs.String("out-dir", "media-manual", "download output directory, relative to state dir unless absolute")
	overwrite := fs.Bool("overwrite", false, "replace an existing downloaded file")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*chat) == "" {
		return fmt.Errorf("--chat is required")
	}
	if cfg.Mode != config.ModeMain {
		if err := ensureAllowedChat(cfg, *chat); err != nil {
			return err
		}
	}
	if *messageID <= 0 {
		return fmt.Errorf("--message-id must be > 0")
	}
	if *index <= 0 {
		return fmt.Errorf("--index must be > 0")
	}
	mediaDir := resolveOutputPath(cfg.StateDir, *outDir)
	var result mtproto.DownloadMediaResult
	if err := withRuntimeLock(cfg, func() error {
		return client.RunAuthorized(context.Background(), func(ctx context.Context, session *mtproto.Session) error {
			var err error
			result, err = session.DownloadMessageMedia(ctx, *chat, *messageID, mtproto.DownloadMediaOptions{
				MediaDir:  mediaDir,
				Index:     *index,
				Overwrite: *overwrite,
			})
			return err
		})
	}); err != nil {
		return err
	}
	if *jsonOut {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	fmt.Fprintf(out, "downloaded=true chat=%d message_id=%d index=%d kind=%s path=%s size=%d\n",
		result.Record.Chat.ID,
		result.Record.MessageID,
		*index,
		result.Attachment.Kind,
		result.Attachment.LocalPath,
		result.Attachment.Size,
	)
	return nil
}

func parseHistoryBounds(fromRaw string, toRaw string) (time.Time, time.Time, error) {
	start, err := parseCompactSince(fromRaw)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--from: %w", err)
	}
	end, err := parseCompactSince(toRaw)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--to: %w", err)
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("--to must be after --from")
	}
	return start, end, nil
}

func runCompact(cfg config.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(out)
	input := fs.String("in", "messages.jsonl", "JSONL input path, relative to state dir unless absolute")
	output := fs.String("out", "messages.toon", "compact TOON-style output path, relative to state dir unless absolute")
	sinceRaw := fs.String("since", "", "optional lower date bound, YYYY-MM-DD or RFC3339")
	limit := fs.Int("limit", 0, "maximum newest records to write after filtering; 0 means all")
	includeService := fs.Bool("include-service", false, "include Telegram service messages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	since, err := parseCompactSince(*sinceRaw)
	if err != nil {
		return err
	}
	inputPath := resolveOutputPath(cfg.StateDir, *input)
	outputPath := resolveOutputPath(cfg.StateDir, *output)
	stats, err := harvest.WriteCompactTOON(harvest.CompactOptions{
		InputPath:      inputPath,
		OutputPath:     outputPath,
		Since:          since,
		Limit:          *limit,
		IncludeService: *includeService,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read=%d wrote=%d skipped=%d in=%s out=%s\n",
		stats.Records,
		stats.Written,
		stats.Skipped,
		inputPath,
		outputPath,
	)
	return nil
}

func runAgentView(cfg config.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent-view", flag.ContinueOnError)
	fs.SetOutput(out)
	input := fs.String("in", "messages.jsonl", "JSONL input path, relative to state dir unless absolute")
	outputDir := fs.String("out-dir", "agent-view", "Markdown agent-view output directory, relative to state dir unless absolute")
	sinceRaw := fs.String("since", "", "optional lower date bound, YYYY-MM-DD or RFC3339")
	recent := fs.Int("recent", 300, "number of newest messages to include in all-recent.md")
	includeService := fs.Bool("include-service", false, "include Telegram service messages")
	rebuild := fs.Bool("rebuild", false, "force full agent-view rebuild instead of incremental update")
	if err := fs.Parse(args); err != nil {
		return err
	}
	since, err := parseCompactSince(*sinceRaw)
	if err != nil {
		return err
	}
	inputPath := resolveOutputPath(cfg.StateDir, *input)
	outputPath := resolveOutputPath(cfg.StateDir, *outputDir)
	opts := harvest.AgentViewOptions{
		InputPath:      inputPath,
		OutputDir:      outputPath,
		Since:          since,
		RecentLimit:    *recent,
		IncludeService: *includeService,
	}
	var stats harvest.AgentViewStats
	if *rebuild {
		stats, err = harvest.WriteAgentMarkdownView(opts)
	} else {
		stats, err = harvest.UpdateAgentMarkdownView(opts)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "mode=%s read=%d wrote=%d skipped=%d raw_added=%d visible_added=%d chats=%d topics=%d files=%d in=%s out=%s\n",
		stats.Mode,
		stats.Records,
		stats.Written,
		stats.Skipped,
		stats.RawAdded,
		stats.VisibleAdded,
		stats.Chats,
		stats.Topics,
		stats.Files,
		inputPath,
		outputPath,
	)
	return nil
}

func printSyncProgress(out io.Writer, progress harvest.SyncProgress) {
	backfill := progress.State.Backfill
	if backfill == nil {
		return
	}
	fmt.Fprintf(out, "progress complete=%t records=%d batch_records=%d batches=%d oldest_id=%d latest_id=%d next_offset_id=%d flood_waits=%d\n",
		backfill.Complete,
		backfill.Records,
		progress.History.BatchRecords,
		backfill.Batches,
		backfill.OldestID,
		backfill.LatestID,
		backfill.NextOffsetID,
		progress.History.FloodWaits,
	)
}

func printInterruptedSync(out io.Writer, statePath string) {
	state, err := harvest.LoadSyncState(statePath)
	if err != nil || state.Backfill == nil {
		fmt.Fprintln(out, "interrupted=true")
		return
	}
	fmt.Fprintf(out, "interrupted=true complete=%t records=%d batches=%d next_offset_id=%d resume=\"sync --all --chat <same> --name <same>\"\n",
		state.Backfill.Complete,
		state.Backfill.Records,
		state.Backfill.Batches,
		state.Backfill.NextOffsetID,
	)
}

func filterAllowedChats(cfg config.Config, chats []harvest.Chat) []harvest.Chat {
	if cfg.AllowedChatCount() == 0 {
		return chats
	}
	filtered := make([]harvest.Chat, 0, len(chats))
	for _, chat := range chats {
		if cfg.ChatAllowed(fmt.Sprintf("%d", chat.ID)) || (chat.Username != "" && cfg.ChatAllowed(chat.Username)) {
			filtered = append(filtered, chat)
		}
	}
	return filtered
}

func ensureAllowedChat(cfg config.Config, chat string) error {
	if cfg.ChatAllowed(chat) {
		return nil
	}
	return fmt.Errorf("chat %q is outside %s; refusing to read outside study scope", chat, cfg.EnvNames("ALLOWED_CHATS"))
}

func withRuntimeLock(cfg config.Config, fn func() error) error {
	lock, err := runlock.Acquire(cfg.RuntimeLockPath())
	if err != nil {
		return err
	}
	defer lock.Release()
	return fn()
}

func resolveOutputPath(stateDir string, output string) string {
	if filepath.IsAbs(output) {
		return output
	}
	return filepath.Join(stateDir, output)
}

func resolveReportDirPath(reportDir string) string {
	reportDir = strings.TrimSpace(reportDir)
	if reportDir == "" || filepath.IsAbs(reportDir) {
		return reportDir
	}
	return filepath.Join(detectProjectRoot(), reportDir)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func parseCompactSince(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	if parsed, err := time.ParseInLocation("2006-01-02", value, moscowLocation()); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("--since must be YYYY-MM-DD or RFC3339")
}

func parseDailyDate(value string, now time.Time) (string, time.Time, time.Time, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "today"
	}
	moscow := moscowLocation()
	now = now.In(moscow)
	var day time.Time
	switch value {
	case "today":
		day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, moscow)
	case "yesterday":
		base := now.AddDate(0, 0, -1)
		day = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, moscow)
	default:
		parsed, err := time.ParseInLocation("2006-01-02", value, moscow)
		if err != nil {
			return "", time.Time{}, time.Time{}, fmt.Errorf("--date must be today, yesterday, or YYYY-MM-DD")
		}
		day = parsed
	}
	dateLabel := day.Format("2006-01-02")
	return dateLabel, day, day.AddDate(0, 0, 1), nil
}

func moscowLocation() *time.Location {
	return time.FixedZone("Europe/Moscow", 3*60*60)
}

func moscowDay(now time.Time) time.Time {
	moscow := moscowLocation()
	now = now.In(moscow)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, moscow)
}

func parseDailyDay(value string) (time.Time, bool) {
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), moscowLocation())
	return day, err == nil
}

func buildDailyCatchupPlan(cfg config.Config, reportDir string, fromRaw string, now time.Time) (dailyCatchupPlan, error) {
	today := moscowDay(now)
	plan := dailyCatchupPlan{Today: today.Format("2006-01-02")}

	var start time.Time
	if strings.TrimSpace(fromRaw) != "" {
		day, ok := parseDailyDay(fromRaw)
		if !ok {
			return dailyCatchupPlan{}, fmt.Errorf("--from must be YYYY-MM-DD")
		}
		start = day
		plan.LastReport = "manual:" + day.AddDate(0, 0, -1).Format("2006-01-02")
	} else {
		latest, ok, err := latestDailyReportDate(reportDir, today)
		if err != nil {
			return dailyCatchupPlan{}, err
		}
		if !ok {
			return dailyCatchupPlan{}, fmt.Errorf("no previous daily Markdown reports found in %s; pass --from YYYY-MM-DD for the first catch-up", reportDir)
		}
		start = latest.AddDate(0, 0, 1)
		plan.LastReport = latest.Format("2006-01-02")
	}

	for day := start; day.Before(today); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		markdownPath := filepath.Join(reportDir, date+".md")
		if fileExists(markdownPath) {
			plan.Skipped = append(plan.Skipped, date)
			continue
		}
		jsonlPath, _ := harvest.DailyDefaultOutputPaths(cfg.StateDir, date)
		plan.Jobs = append(plan.Jobs, dailyJob{
			Date:         date,
			Start:        day,
			End:          day.AddDate(0, 0, 1),
			OutputPath:   jsonlPath,
			MarkdownPath: markdownPath,
			ASRLogPath:   harvest.DailyDefaultASRLogPath(cfg.StateDir, date),
		})
	}
	return plan, nil
}

func latestDailyReportDate(reportDir string, before time.Time) (time.Time, bool, error) {
	entries, err := os.ReadDir(reportDir)
	if os.IsNotExist(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	var latest time.Time
	found := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".md" {
			continue
		}
		day, ok := parseDailyDay(strings.TrimSuffix(name, filepath.Ext(name)))
		if !ok || !day.Before(before) {
			continue
		}
		if !found || day.After(latest) {
			latest = day
			found = true
		}
	}
	return latest, found, nil
}

func dailyDialogLimitDefault() int {
	return mtproto.DefaultDailyDialogLimit()
}

func dailyAdditionalSenderIDsByChat(cfg config.Config) map[int64][]int64 {
	if len(cfg.DailyAdditionalSenders) == 0 {
		return nil
	}
	result := make(map[int64][]int64)
	for _, source := range cfg.DailyAdditionalSenders {
		result[source.ChatID] = append(result[source.ChatID], source.SenderID)
	}
	return result
}

func dailyDialogCheckpointScopeFingerprint(cfg config.Config, opts dailyOptions) string {
	dialogLimit := opts.DialogLimit
	if dialogLimit <= 0 {
		dialogLimit = dailyDialogLimitDefault()
	}
	scope := harvest.DailyDialogCheckpointScope{
		Version:        harvest.DailyDialogCheckpointVersion,
		DialogLimit:    dialogLimit,
		IncludeService: opts.IncludeService,
	}
	for _, sender := range cfg.DailyAdditionalSenders {
		scope.AdditionalSenders = append(scope.AdditionalSenders, harvest.DailyDialogScopeSenderRef{
			ChatID:   sender.ChatID,
			SenderID: sender.SenderID,
		})
	}
	return harvest.DailyDialogScopeFingerprint(scope)
}

func dailyTranscribeOptions(opts harvest.HistoryOptions) transcribe.Options {
	return transcribe.ProductionOptions(
		opts.WhisperCommand,
		opts.WhisperModelPath,
		opts.WhisperGateFilePath,
		opts.FFmpegCommand,
		opts.StageTiming,
	)
}

const (
	transcribeFileContractVersion    = 4
	transcribeProfileAdaptiveMedia   = "adaptive-media-v1"
	transcribeValidationRuntimeReady = "runtime-ready"
	transcribeValidationTranscribed  = "transcribed"
	transcribeValidationNoSpeech     = "no-speech"
	transcribeValidationCoverage     = "coverage-validated"
)

type transcribeFileResponse struct {
	ContractVersion   int                     `json:"contract_version"`
	Status            string                  `json:"status"`
	ProfileID         string                  `json:"profile_id"`
	ValidationStatus  string                  `json:"validation_status"`
	Text              string                  `json:"text,omitempty"`
	SpeechDetected    bool                    `json:"speech_detected"`
	MetalConfirmed    bool                    `json:"metal_confirmed"`
	Engine            string                  `json:"engine"`
	Backend           transcribe.Descriptor   `json:"backend"`
	FFmpeg            time.Duration           `json:"ffmpeg"`
	ModelColdStart    time.Duration           `json:"model_cold_start"`
	SpeechGate        time.Duration           `json:"speech_gate"`
	LongFormPrep      time.Duration           `json:"long_form_preparation"`
	LanguageDetection time.Duration           `json:"language_detection"`
	LeadingOffset     float64                 `json:"leading_speech_offset_seconds,omitempty"`
	Diagnostics       *transcribe.Diagnostics `json:"diagnostics,omitempty"`
	Strategy          string                  `json:"strategy,omitempty"`
	RouteReason       string                  `json:"route_reason,omitempty"`
	Inference         time.Duration           `json:"inference"`
	Total             time.Duration           `json:"total"`
}

func runTranscribeFile(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("transcribe-file", flag.ContinueOnError)
	fs.SetOutput(out)
	inputPath := fs.String("input", "", "local audio or video input path")
	outputPath := fs.String("output", "", "plain UTF-8 transcript output path")
	check := fs.Bool("check", false, "validate the configured production ASR runtime and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("transcribe-file takes no positional arguments")
	}
	defaults := dailyRuntimeDefaults()
	opts := transcribe.ProductionOptions(
		defaults.WhisperCommand,
		defaults.WhisperModelPath,
		defaults.WhisperGateFilePath,
		defaults.FFmpegCommand,
		nil,
	)
	if err := opts.ValidateRuntime(); err != nil {
		return fmt.Errorf("production ASR runtime: %w", err)
	}
	if *check {
		return writeTranscribeFileResponse(out, transcribeFileResponse{
			ContractVersion:  transcribeFileContractVersion,
			Status:           "ok",
			ProfileID:        transcribeProfileAdaptiveMedia,
			ValidationStatus: transcribeValidationRuntimeReady,
			Engine:           opts.EngineName(),
			Backend:          opts.Descriptor(),
		})
	}
	if strings.TrimSpace(*inputPath) == "" || strings.TrimSpace(*outputPath) == "" {
		return fmt.Errorf("transcribe-file requires --input and --output")
	}
	inputAbs, err := filepath.Abs(*inputPath)
	if err != nil {
		return fmt.Errorf("resolve transcribe input: %w", err)
	}
	outputAbs, err := filepath.Abs(*outputPath)
	if err != nil {
		return fmt.Errorf("resolve transcript output: %w", err)
	}
	if samePath(inputAbs, outputAbs) {
		return fmt.Errorf("transcript output must differ from media input")
	}
	info, err := os.Stat(inputAbs)
	if err != nil {
		return fmt.Errorf("inspect transcribe input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("transcribe input must be a non-empty regular file")
	}
	result, err := transcribe.RunDetailed(ctx, opts, inputAbs, outputAbs)
	if err != nil {
		return err
	}
	speechDetected := result.SpeechDetected
	validationStatus := transcribeValidationTranscribed
	if !speechDetected {
		validationStatus = transcribeValidationNoSpeech
	}
	if result.Strategy == transcribe.StrategyLongForm {
		if result.Diagnostics == nil || !result.Diagnostics.CoverageValidated {
			return fmt.Errorf("adaptive long-form result did not prove trailing speech coverage")
		}
		validationStatus = transcribeValidationCoverage
	}
	inference := result.ASRDuration - result.SpeechGateDuration - result.LongFormPreparationDuration
	if inference < 0 {
		inference = 0
	}
	return writeTranscribeFileResponse(out, transcribeFileResponse{
		ContractVersion:   transcribeFileContractVersion,
		Status:            "ok",
		ProfileID:         transcribeProfileAdaptiveMedia,
		ValidationStatus:  validationStatus,
		Text:              result.Text,
		SpeechDetected:    speechDetected,
		MetalConfirmed:    speechDetected,
		Engine:            result.Engine,
		Backend:           result.Backend,
		FFmpeg:            result.FFmpegDuration,
		ModelColdStart:    result.ModelColdStartDuration,
		SpeechGate:        result.SpeechGateDuration,
		LongFormPrep:      result.LongFormPreparationDuration,
		LanguageDetection: result.LanguageDetectionDuration,
		LeadingOffset:     result.LeadingSpeechOffset,
		Diagnostics:       result.Diagnostics,
		Strategy:          result.Strategy,
		RouteReason:       result.RouteReason,
		Inference:         inference,
		Total:             result.TotalDuration,
	})
}

func writeTranscribeFileResponse(out io.Writer, value transcribeFileResponse) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

type dailyRuntimeConfig struct {
	TranscribeMedia     bool
	WhisperCommand      string
	WhisperModelPath    string
	WhisperGateFilePath string
	FFmpegCommand       string
}

func dailyRuntimeDefaults() dailyRuntimeConfig {
	ffmpegCommand := firstEnvValue("TG_HARVEST_DAILY_FFMPEG_COMMAND")
	if ffmpegCommand == "" {
		ffmpegCommand = transcribe.DefaultFFmpegCommand
	}
	whisperCommand := firstEnvValue("TG_HARVEST_DAILY_WHISPER_COMMAND")
	if whisperCommand == "" {
		whisperCommand = defaultLocalWhisperCommandPath()
	}
	whisperModelPath := firstEnvValue("TG_HARVEST_DAILY_WHISPER_MODEL_PATH")
	if whisperModelPath == "" {
		whisperModelPath = defaultLocalWhisperModelPath()
	}
	whisperSpeechGatePath := firstEnvValue("TG_HARVEST_DAILY_WHISPER_SPEECH_GATE_MODEL_PATH")
	if whisperSpeechGatePath == "" {
		whisperSpeechGatePath = defaultLocalWhisperSpeechGateModelPath()
	}
	configured := strings.TrimSpace(whisperCommand) != "" &&
		strings.TrimSpace(whisperModelPath) != "" &&
		strings.TrimSpace(whisperSpeechGatePath) != ""
	return dailyRuntimeConfig{
		TranscribeMedia:     configured,
		WhisperCommand:      whisperCommand,
		WhisperModelPath:    whisperModelPath,
		WhisperGateFilePath: whisperSpeechGatePath,
		FFmpegCommand:       ffmpegCommand,
	}
}

func defaultLocalWhisperCommandPath() string {
	projectRoot := detectProjectRoot()
	if projectRoot == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(projectRoot, "bin", "whisper-server-metal"),
		filepath.Join(projectRoot, ".state", "asr-runtime", "whisper.cpp", "build-metal", "bin", "whisper-server"),
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

func defaultLocalWhisperModelPath() string {
	projectRoot := detectProjectRoot()
	if projectRoot == "" {
		return ""
	}
	candidate := filepath.Join(projectRoot, ".state", "asr-runtime", "whisper.cpp", "models", "ggml-large-v3-turbo-q5_0.bin")
	if fileExists(candidate) {
		return candidate
	}
	return ""
}

func defaultLocalWhisperSpeechGateModelPath() string {
	projectRoot := detectProjectRoot()
	if projectRoot == "" {
		return ""
	}
	candidate := filepath.Join(projectRoot, ".state", "asr-runtime", "whisper.cpp", "models", "ggml-silero-v6.2.0.bin")
	if fileExists(candidate) {
		return candidate
	}
	return ""
}

func firstEnvValue(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func resolveCommand(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", false
	}
	if resolved, err := exec.LookPath(command); err == nil {
		return resolved, true
	}
	if fileExists(command) {
		return command, true
	}
	return "", false
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func detectProjectRoot() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
		if fileExists(filepath.Join(root, "go.mod")) {
			return root
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func loadToolDotEnv(projectRoot string) error {
	cwd, err := os.Getwd()
	if err == nil {
		if err := config.LoadDotEnv(filepath.Join(cwd, ".env")); err != nil {
			return err
		}
	}
	if projectRoot == "" {
		return nil
	}
	rootEnv := filepath.Join(projectRoot, ".env")
	if err == nil && samePath(cwd, projectRoot) {
		return nil
	}
	return config.LoadDotEnv(rootEnv)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftAbs, err := filepath.Abs(left)
	if err != nil {
		return false
	}
	rightAbs, err := filepath.Abs(right)
	if err != nil {
		return false
	}
	return leftAbs == rightAbs
}
