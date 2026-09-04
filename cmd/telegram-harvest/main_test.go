package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chupakobra6/telegram-harvest/internal/config"
	"github.com/chupakobra6/telegram-harvest/internal/harvest"
	"github.com/chupakobra6/telegram-harvest/internal/mtproto"
	"github.com/chupakobra6/telegram-harvest/internal/stages"
	"github.com/chupakobra6/telegram-harvest/internal/transcribe"
)

func TestRunHelpPrintsCommands(t *testing.T) {
	code, stdout, stderr := runCommand(t, []string{"help"}, nil)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"agent-view",
		"compact",
		"download-media --chat",
		"daily-catchup",
		"daily-download-media --chat",
		"transcribe-file --input",
		"send-saved --text",
		"@Pheik13 main session -> InputPeerSelf only",
		"--profile main|study",
		"required account profile",
		"Harvesting operations are read-only",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("help missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "import-tdesktop") {
		t.Fatalf("help must not expose Telegram Desktop import:\n%s", stdout)
	}
}

func TestRunSendSavedHasNoRecipientOption(t *testing.T) {
	client := mtproto.New(config.Config{Mode: config.ModeMain})
	var out strings.Builder
	err := runSendSaved(config.Config{Mode: config.ModeMain}, client, []string{"--recipient", "@someone", "--text", "test"}, &out)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -recipient") {
		t.Fatalf("err=%v output=%s", err, out.String())
	}
}

func TestRunTranscribeFileCheckUsesProductionRuntime(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	whisperCommand := filepath.Join(binDir, "whisper-server")
	gateCommand := filepath.Join(binDir, "whisper-vad-speech-segments")
	ffmpegCommand := filepath.Join(binDir, "ffmpeg")
	for _, path := range []string{whisperCommand, gateCommand, ffmpegCommand} {
		mustWriteCLIFile(t, path, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	modelPath := filepath.Join(dir, transcribe.ProductionModelFile)
	gateModelPath := filepath.Join(dir, transcribe.ProductionSpeechGateFile)
	mustWriteCLIFile(t, modelPath, "model")
	mustWriteCLIFile(t, gateModelPath, "gate")
	env := map[string]string{
		"TG_HARVEST_DAILY_WHISPER_COMMAND":                whisperCommand,
		"TG_HARVEST_DAILY_WHISPER_MODEL_PATH":             modelPath,
		"TG_HARVEST_DAILY_WHISPER_SPEECH_GATE_MODEL_PATH": gateModelPath,
		"TG_HARVEST_DAILY_FFMPEG_COMMAND":                 ffmpegCommand,
	}
	code, stdout, stderr := runCommand(t, []string{"--profile", "main", "transcribe-file", "--check"}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var response transcribeFileResponse
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("decode response: %v\n%s", err, stdout)
	}
	if response.ContractVersion != transcribeFileContractVersion || response.Status != "ok" ||
		response.ProfileID != transcribeProfileAdaptiveMedia || response.ValidationStatus != transcribeValidationRuntimeReady {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.Backend.Backend != transcribe.BackendWhisperCPP || response.Backend.Accelerator != transcribe.AcceleratorMetal {
		t.Fatalf("unexpected backend: %+v", response.Backend)
	}
	if response.Backend.Decode == nil || response.Backend.Decode.BeamSize != 5 {
		t.Fatalf("production decode profile missing: %+v", response.Backend.Decode)
	}
	if response.Backend.SpeechGate == nil || response.Backend.Adaptive == nil ||
		response.Backend.Adaptive.RoutingPolicy == "" || response.Backend.Adaptive.RussianInitialPrompt == "" {
		t.Fatalf("adaptive production policy missing: %+v", response.Backend)
	}
}

func TestRunTranscribeFileRejectsRemovedTrustedLongFormMode(t *testing.T) {
	code, _, stderr := runCommand(t, []string{"--profile", "main", "transcribe-file", "--check", "--trusted-long-form"}, nil)
	if code != 1 || !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestRunTranscribeFileRejectsRemovedAssumeSpeechMode(t *testing.T) {
	code, _, stderr := runCommand(t, []string{"--profile", "main", "transcribe-file", "--check", "--assume-speech"}, nil)
	if code != 1 || !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestRunTranscribeFileRejectsStudyProfile(t *testing.T) {
	code, _, stderr := runCommand(t, []string{"--profile", "study", "transcribe-file", "--check"}, nil)
	if code != 1 || !strings.Contains(stderr, "only for profile main") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestRunCommandHelpExitsSuccessfully(t *testing.T) {
	code, stdout, stderr := runCommand(t, []string{"--profile", "main", "daily", "--help"}, nil)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"-transcribe", "-whisper-command", "-whisper-model", "-whisper-speech-gate-model"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("daily help missing %q:\n%s", want, stdout)
		}
	}
	for _, removed := range []string{"-asr-backend", "-asr-workers", "-vosk-command", "-transcribe-cmd"} {
		if strings.Contains(stdout, removed) {
			t.Fatalf("daily help exposes removed flag %q:\n%s", removed, stdout)
		}
	}
}

func TestRunRejectsTelegramDesktopImportCommand(t *testing.T) {
	code, _, stderr := runCommand(t, []string{"import-tdesktop"}, nil)
	if code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "unknown command: import-tdesktop") {
		t.Fatalf("missing unknown command error: %s", stderr)
	}
}

func TestRunRequiresExplicitProfile(t *testing.T) {
	code, _, stderr := runCommand(t, []string{"doctor"}, nil)
	if code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "--profile main|study is required") {
		t.Fatalf("missing profile error: %s", stderr)
	}
}

func TestRunPrintConfigUsesEnvAndRootedPaths(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"TG_HARVEST_STUDY_STATE_DIR":     filepath.Join(dir, "state"),
		"TG_HARVEST_STUDY_SESSION_PATH":  filepath.Join(dir, "sessions", "study.json"),
		"TG_HARVEST_STUDY_ALLOWED_CHATS": "12345,@study",
	}
	code, stdout, stderr := runCommand(t, []string{"print-config", "--profile", "study"}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"profile=study",
		"state_dir=" + filepath.Join(dir, "state"),
		"session=" + filepath.Join(dir, "sessions", "study.json"),
		"allowed_chats=2",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("print-config missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunPrintConfigCanSelectMainProfile(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"TG_HARVEST_DAILY_APP_ID":                         "77",
		"TG_HARVEST_DAILY_APP_HASH":                       "main-hash",
		"TG_HARVEST_DAILY_STATE_DIR":                      filepath.Join(dir, "main-state"),
		"TG_HARVEST_DAILY_SESSION_PATH":                   filepath.Join(dir, "main-session.json"),
		"TG_HARVEST_DAILY_WHISPER_COMMAND":                "/tmp/whisper-server",
		"TG_HARVEST_DAILY_WHISPER_MODEL_PATH":             filepath.Join(dir, "ggml-large-v3-turbo-q5_0.bin"),
		"TG_HARVEST_DAILY_WHISPER_SPEECH_GATE_MODEL_PATH": filepath.Join(dir, "ggml-silero-v6.2.0.bin"),
	}
	code, stdout, stderr := runCommand(t, []string{"print-config", "--profile", "main"}, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"profile=main",
		"app_id_set=true",
		"state_dir=" + filepath.Join(dir, "main-state"),
		"session=" + filepath.Join(dir, "main-session.json"),
		"daily_asr_backend=whispercpp",
		"daily_whisper_accelerator=metal",
		"daily_asr_language=auto",
		"daily_asr_workers=1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("main print-config missing %q:\n%s", want, stdout)
		}
	}
}

func TestMaskCLIPhone(t *testing.T) {
	if got := maskCLIPhone("10000000017"); got != "+1********17" {
		t.Fatalf("masked phone = %s", got)
	}
	if got := maskCLIPhone("+1234"); got != "+1234" {
		t.Fatalf("short masked phone = %s", got)
	}
}

func TestRunCompactAndAgentViewUseStateDirRelativePaths(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	input := filepath.Join(stateDir, "messages.jsonl")
	mustWriteCLIFile(t, input, strings.Join([]string{
		`{"source":"telegram","chat":{"id":123,"display":"Study"},"message_id":1,"date":"2026-05-10T10:00:00Z","sender":{"display":"Student"},"kind":"text","text":"first"}`,
		`{"source":"telegram","chat":{"id":123,"display":"Study"},"message_id":2,"date":"2026-05-10T11:00:00Z","sender":{"display":"Teacher"},"kind":"text","text":"second"}`,
	}, "\n")+"\n")
	env := map[string]string{
		"TG_HARVEST_STUDY_STATE_DIR":    stateDir,
		"TG_HARVEST_STUDY_SESSION_PATH": filepath.Join(dir, "sessions", "study.json"),
	}

	code, stdout, stderr := runCommand(t, []string{"--profile", "study", "compact", "--in", "messages.jsonl", "--out", "messages.toon"}, env)
	if code != 0 {
		t.Fatalf("compact code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "wrote=2") {
		t.Fatalf("unexpected compact stdout:\n%s", stdout)
	}
	toon := readCLIFile(t, filepath.Join(stateDir, "messages.toon"))
	if !strings.Contains(toon, "messages[2|]") || !strings.Contains(toon, "Teacher|second") {
		t.Fatalf("compact output missing records:\n%s", toon)
	}

	code, stdout, stderr = runCommand(t, []string{"--profile", "study", "agent-view", "--in", "messages.jsonl", "--out-dir", "agent-view", "--recent", "5"}, env)
	if code != 0 {
		t.Fatalf("agent-view code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "mode=rebuild") || !strings.Contains(stdout, "visible_added=2") {
		t.Fatalf("unexpected agent-view stdout:\n%s", stdout)
	}
	index := readCLIFile(t, filepath.Join(stateDir, "agent-view", "README.md"))
	if !strings.Contains(index, "Study") || !strings.Contains(index, "Total visible messages: `2`") {
		t.Fatalf("agent-view index missing summary:\n%s", index)
	}

	code, stdout, stderr = runCommand(t, []string{"--profile", "study", "agent-view", "--in", "messages.jsonl", "--out-dir", "agent-view", "--recent", "5"}, env)
	if code != 0 {
		t.Fatalf("agent-view noop code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "mode=noop") || !strings.Contains(stdout, "visible_added=0") {
		t.Fatalf("expected noop stdout:\n%s", stdout)
	}
}

func TestRunReadCommandsRefuseChatsOutsideAllowlistBeforeRuntimeAccess(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"TG_HARVEST_STUDY_STATE_DIR":     filepath.Join(dir, "state"),
		"TG_HARVEST_STUDY_SESSION_PATH":  filepath.Join(dir, "sessions", "study.json"),
		"TG_HARVEST_STUDY_ALLOWED_CHATS": "12345",
	}
	for _, args := range [][]string{
		{"--profile", "study", "topics", "--chat", "999"},
		{"--profile", "study", "dump", "--chat", "999", "--out", "x.jsonl"},
		{"--profile", "study", "sync", "--chat", "999", "--name", "x"},
		{"--profile", "study", "download-media", "--chat", "999", "--message-id", "1"},
	} {
		code, _, stderr := runCommand(t, args, env)
		if code != 1 {
			t.Fatalf("%v code=%d stderr=%s", args, code, stderr)
		}
		if !strings.Contains(stderr, "outside TG_HARVEST_STUDY_ALLOWED_CHATS") {
			t.Fatalf("%v missing allowlist error: %s", args, stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "sessions", "study.json.runtime.lock")); !os.IsNotExist(err) {
			t.Fatalf("%v should not acquire runtime lock, stat err=%v", args, err)
		}
	}
}

func TestParseCompactSinceUsesMoscowForDateOnly(t *testing.T) {
	got, err := parseCompactSince("2026-05-14")
	if err != nil {
		t.Fatalf("parse since: %v", err)
	}
	if got.Location().String() != "Europe/Moscow" {
		t.Fatalf("expected Europe/Moscow location, got %s", got.Location())
	}
	if got.Format("2006-01-02T15:04:05-07:00") != "2026-05-14T00:00:00+03:00" {
		t.Fatalf("unexpected parsed value: %s", got.Format("2006-01-02T15:04:05-07:00"))
	}
	if _, err := parseCompactSince("14-05-2026"); err == nil {
		t.Fatalf("expected invalid date error")
	}
}

func TestRunDumpRejectsInvalidDateBoundsBeforeRuntimeAccess(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions", "study.json")
	env := map[string]string{
		"TG_HARVEST_STUDY_STATE_DIR":    filepath.Join(dir, "state"),
		"TG_HARVEST_STUDY_SESSION_PATH": sessionPath,
	}

	code, _, stderr := runCommand(t, []string{"--profile", "study", "dump", "--chat", "12345", "--out", "x.jsonl", "--from", "bad-date"}, env)
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "--from") {
		t.Fatalf("missing --from validation error: %s", stderr)
	}
	if _, err := os.Stat(sessionPath + ".runtime.lock"); !os.IsNotExist(err) {
		t.Fatalf("dump should not acquire runtime lock, stat err=%v", err)
	}
}

func TestRunDumpRejectsInvalidVideoTranscriptionModeBeforeRuntimeAccess(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions", "main.json")
	env := map[string]string{
		"TG_HARVEST_DAILY_STATE_DIR":    filepath.Join(dir, "state"),
		"TG_HARVEST_DAILY_SESSION_PATH": sessionPath,
	}

	code, _, stderr := runCommand(t, []string{"--profile", "main", "dump", "--chat", "12345", "--out", "x.jsonl", "--transcribe-video", "cinema"}, env)
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "--transcribe-video must be one of: phone, all, off") {
		t.Fatalf("missing --transcribe-video validation error: %s", stderr)
	}
	if _, err := os.Stat(sessionPath + ".runtime.lock"); !os.IsNotExist(err) {
		t.Fatalf("dump should not acquire runtime lock, stat err=%v", err)
	}
}

func TestRunDumpKeepsStudyTranscriptionDisabled(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions", "study.json")
	env := map[string]string{
		"TG_HARVEST_STUDY_STATE_DIR":    filepath.Join(dir, "state"),
		"TG_HARVEST_STUDY_SESSION_PATH": sessionPath,
	}

	code, _, stderr := runCommand(t, []string{"--profile", "study", "dump", "--chat", "12345", "--out", "x.jsonl", "--transcribe"}, env)
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "dump --transcribe is supported only for profile main") {
		t.Fatalf("missing profile restriction error: %s", stderr)
	}
	if _, err := os.Stat(sessionPath + ".runtime.lock"); !os.IsNotExist(err) {
		t.Fatalf("dump should not acquire runtime lock, stat err=%v", err)
	}
}

func TestParseDailyDateSupportsRelativeDays(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	date, start, end, err := parseDailyDate("yesterday", now)
	if err != nil {
		t.Fatalf("parse daily date: %v", err)
	}
	if date != "2026-06-04" {
		t.Fatalf("date=%s", date)
	}
	if start.Format("2006-01-02T15:04:05-07:00") != "2026-06-04T00:00:00+03:00" {
		t.Fatalf("start=%s", start.Format("2006-01-02T15:04:05-07:00"))
	}
	if end.Sub(start) != 24*time.Hour {
		t.Fatalf("end-start=%s", end.Sub(start))
	}
}

func TestBuildDailyCatchupPlanStartsAfterLatestReportAndSkipsToday(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports", "daily")
	mustWriteCLIFile(t, filepath.Join(reportDir, "2026-06-02.md"), "done")
	mustWriteCLIFile(t, filepath.Join(reportDir, "2026-06-07.md"), "today partial")
	cfg := configForCatchup(root)

	plan, err := buildDailyCatchupPlan(cfg, reportDir, "", time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.LastReport != "2026-06-02" || plan.Today != "2026-06-07" {
		t.Fatalf("unexpected plan labels: %+v", plan)
	}
	got := dailyJobDates(plan.Jobs)
	want := []string{"2026-06-03", "2026-06-04", "2026-06-05", "2026-06-06"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("jobs=%v want=%v", got, want)
	}
	for _, job := range plan.Jobs {
		if job.MarkdownPath != filepath.Join(reportDir, job.Date+".md") {
			t.Fatalf("markdown path for %s = %s", job.Date, job.MarkdownPath)
		}
		if !strings.HasSuffix(job.OutputPath, filepath.Join(".state", "daily", "jsonl", job.Date+".jsonl")) {
			t.Fatalf("jsonl path for %s = %s", job.Date, job.OutputPath)
		}
	}
}

func TestBuildDailyCatchupPlanSkipsExistingReportsFromManualStart(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports", "daily")
	mustWriteCLIFile(t, filepath.Join(reportDir, "2026-06-04.md"), "done")
	cfg := configForCatchup(root)

	plan, err := buildDailyCatchupPlan(cfg, reportDir, "2026-06-03", time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := dailyJobDates(plan.Jobs)
	want := []string{"2026-06-03", "2026-06-05", "2026-06-06"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("jobs=%v want=%v", got, want)
	}
	if strings.Join(plan.Skipped, ",") != "2026-06-04" {
		t.Fatalf("skipped=%v", plan.Skipped)
	}
	if got := strings.Join(dailyCatchupDates(plan), ","); got != "2026-06-03,2026-06-04,2026-06-05,2026-06-06" {
		t.Fatalf("merged dates=%s", got)
	}
}

func TestBuildDailyCatchupPlanRequiresManualStartWithoutReports(t *testing.T) {
	root := t.TempDir()
	cfg := configForCatchup(root)
	_, err := buildDailyCatchupPlan(cfg, filepath.Join(root, "reports", "daily"), "", time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected missing report error")
	}
	if !strings.Contains(err.Error(), "--from YYYY-MM-DD") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDailyOptionsRejectsInvalidVideoTranscribeMode(t *testing.T) {
	if err := validateDailyOptions(dailyOptions{VideoTranscribeMode: "phone"}); err != nil {
		t.Fatalf("phone mode rejected: %v", err)
	}
	err := validateDailyOptions(dailyOptions{VideoTranscribeMode: "cinema"})
	if err == nil || !strings.Contains(err.Error(), "--transcribe-video") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateDailyOptionsChecksProductionWhisperRuntime(t *testing.T) {
	valid := dailyOptions{
		TranscribeMedia:     true,
		VideoTranscribeMode: harvest.VideoTranscribePhone,
		WhisperCommand:      "whisper-server",
		WhisperModelPath:    "ggml-large-v3-turbo-q5_0.bin",
		WhisperGateFilePath: "ggml-silero-v6.2.0.bin",
	}
	if err := validateDailyOptions(valid); err != nil {
		t.Fatal(err)
	}
	missingModel := valid
	missingModel.WhisperModelPath = ""
	if err := validateDailyOptions(missingModel); err == nil || !strings.Contains(err.Error(), "model path") {
		t.Fatalf("missing model error = %v", err)
	}
	missingGate := valid
	missingGate.WhisperGateFilePath = ""
	if err := validateDailyOptions(missingGate); err == nil || !strings.Contains(err.Error(), "speech-gate model") {
		t.Fatalf("missing speech gate error = %v", err)
	}
}

func TestDailyTranscribeOptionsUsesOnlyPinnedWhisperProfile(t *testing.T) {
	whisper := dailyTranscribeOptions(harvest.HistoryOptions{
		WhisperCommand:      "whisper-server",
		WhisperModelPath:    "ggml-large-v3-turbo-q5_0.bin",
		WhisperGateFilePath: "ggml-silero-v6.2.0.bin",
	})
	descriptor := whisper.Descriptor()
	if descriptor.Decode == nil || descriptor.Decode.BeamSize != 5 || descriptor.SpeechGate == nil {
		t.Fatalf("daily whisper descriptor = %#v", descriptor)
	}
	if descriptor.Backend != transcribe.BackendWhisperCPP || descriptor.Accelerator != transcribe.AcceleratorMetal {
		t.Fatalf("daily whisper backend = %#v", descriptor)
	}
}

func TestRunDailyCatchupRejectsInvalidVideoTranscribeModeBeforeRuntimeAccess(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions", "main.json")
	env := map[string]string{
		"TG_HARVEST_DAILY_APP_ID":       "77",
		"TG_HARVEST_DAILY_APP_HASH":     "main-hash",
		"TG_HARVEST_DAILY_STATE_DIR":    filepath.Join(dir, "state"),
		"TG_HARVEST_DAILY_SESSION_PATH": sessionPath,
	}

	code, _, stderr := runCommand(t, []string{"--profile", "main", "daily-catchup", "--transcribe-video", "cinema"}, env)
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "--transcribe-video must be one of: phone, all, off") {
		t.Fatalf("missing video mode validation error: %s", stderr)
	}
	if _, err := os.Stat(sessionPath + ".runtime.lock"); !os.IsNotExist(err) {
		t.Fatalf("daily-catchup should not acquire runtime lock, stat err=%v", err)
	}
}

func TestRunDailyRangeJobsUsesOneScanAndPartitionsReports(t *testing.T) {
	root := t.TempDir()
	location := time.FixedZone("MSK", 3*60*60)
	dayOne := time.Date(2026, 6, 3, 0, 0, 0, 0, location)
	dayThree := dayOne.AddDate(0, 0, 2)
	jobs := []dailyJob{
		testDailyJob(root, "2026-06-05", dayThree),
		testDailyJob(root, "2026-06-03", dayOne),
	}
	dumper := &fakeDailyRangeDumper{
		records: []harvest.MessageRecord{
			{Chat: harvest.Chat{ID: 1, Display: "One"}, MessageID: 1, Date: dayOne.Add(time.Hour), Outgoing: true, Kind: "text", Text: "old"},
			{Chat: harvest.Chat{ID: 1, Display: "One"}, MessageID: 2, Date: dayOne.Add(2 * time.Hour), Outgoing: true, Kind: "text", Text: "latest"},
			{Chat: harvest.Chat{ID: 2, Display: "Gap"}, MessageID: 3, Date: dayOne.AddDate(0, 0, 1).Add(time.Hour), Outgoing: true, Kind: "text", Text: "existing day"},
			{Chat: harvest.Chat{ID: 3, Display: "Three"}, MessageID: 4, Date: dayThree.Add(time.Hour), Outgoing: true, Kind: "text", Text: "third"},
		},
		stats: harvest.OutgoingStats{
			DialogsScanned: 4,
			Batches:        5,
			Complete:       true,
		},
	}
	var output strings.Builder
	timings := newDailyStageTimingCollector("daily-catchup", "2026-06-03", "2026-06-05")
	_, err := runDailyRangeJobs(
		context.Background(),
		dumper,
		harvest.HistoryOptions{Limit: 99, StageTiming: timings.Observe},
		dailyOptions{Limit: 1},
		jobs,
		map[int64][]int64{10: {20}},
		&output,
	)
	if err != nil {
		t.Fatal(err)
	}
	if dumper.calls != 1 {
		t.Fatalf("range calls = %d, want 1", dumper.calls)
	}
	if !dumper.opts.Start.Equal(dayOne) || !dumper.opts.End.Equal(dayThree.AddDate(0, 0, 1)) {
		t.Fatalf("range = %s..%s", dumper.opts.Start, dumper.opts.End)
	}
	if dumper.opts.History.Limit != 0 {
		t.Fatalf("range history limit = %d, want 0 before per-day limiting", dumper.opts.History.Limit)
	}
	if dumper.opts.History.StageTiming == nil {
		t.Fatal("range scan lost stage timing observer")
	}
	if got := dumper.opts.AdditionalSenderIDsByChat[10]; len(got) != 1 || got[0] != 20 {
		t.Fatalf("additional senders = %#v", dumper.opts.AdditionalSenderIDsByChat)
	}

	dayOneRecords := readMessageRecords(t, jobs[1].OutputPath)
	if len(dayOneRecords) != 1 || dayOneRecords[0].MessageID != 2 {
		t.Fatalf("day one records = %+v", dayOneRecords)
	}
	dayThreeRecords := readMessageRecords(t, jobs[0].OutputPath)
	if len(dayThreeRecords) != 1 || dayThreeRecords[0].MessageID != 4 {
		t.Fatalf("day three records = %+v", dayThreeRecords)
	}
	if strings.Contains(readCLIFile(t, jobs[1].MarkdownPath), "existing day") {
		t.Fatal("gap-day record leaked into generated report")
	}
	if got := readASREvents(t, jobs[1].ASRLogPath); len(got) != 2 {
		t.Fatalf("day one ASR events = %d, want 2", len(got))
	}
	if got := readASREvents(t, jobs[0].ASRLogPath); len(got) != 1 || got[0].MessageID != 4 {
		t.Fatalf("day three ASR events = %+v", got)
	}
	if !strings.Contains(output.String(), "range start=2026-06-03 end=2026-06-05") {
		t.Fatalf("missing range summary:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "collected=3 published=2") {
		t.Fatalf("range summary does not distinguish collected and limited records:\n%s", output.String())
	}
	if got := timings.durations[stages.Render]; got <= 0 {
		t.Fatalf("render timing = %s", got)
	}
}

func TestRunDailyRangeJobsDoesNotPublishIncompleteRange(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	job := testDailyJob(root, "2026-06-03", start)
	mustWriteCLIFile(t, job.OutputPath, "old-jsonl\n")
	mustWriteCLIFile(t, job.MarkdownPath, "old-markdown\n")
	checkpointPath := harvest.DailyDialogCheckpointPath(root)
	oldCheckpoint := harvest.NewDailyDialogCheckpoint(42, "scope", "2026-06-02", []harvest.DailyDialogHead{
		{ChatID: 1, ChatType: "user", TopMessageID: 1, VerifiedMessageID: 1, HeadFullyVerified: true},
	}, time.Unix(1, 0))
	if err := harvest.SaveDailyDialogCheckpoint(checkpointPath, oldCheckpoint); err != nil {
		t.Fatal(err)
	}
	dumper := &fakeDailyRangeDumper{
		records: []harvest.MessageRecord{
			{Chat: harvest.Chat{ID: 1}, MessageID: 1, Date: start.Add(time.Hour), Outgoing: true, Kind: "text"},
		},
		stats: harvest.OutgoingStats{
			DialogsScanned: 1,
			DialogErrors:   []string{"One (1): timeout"},
			Complete:       false,
		},
	}

	_, err := runDailyRangeJobs(context.Background(), dumper, harvest.HistoryOptions{}, dailyOptions{}, []dailyJob{job}, nil, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v", err)
	}
	if got := readCLIFile(t, job.OutputPath); got != "old-jsonl\n" {
		t.Fatalf("JSONL was replaced: %q", got)
	}
	if got := readCLIFile(t, job.MarkdownPath); got != "old-markdown\n" {
		t.Fatalf("Markdown was replaced: %q", got)
	}
	checkpoint, err := harvest.LoadDailyDialogCheckpoint(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.VerifiedThrough != oldCheckpoint.VerifiedThrough || checkpoint.Dialogs[0].TopMessageID != 1 {
		t.Fatalf("checkpoint changed after incomplete range: %+v", checkpoint)
	}
}

func TestPublishDailyCatchupCompletionCommitsCheckpointOnlyAfterMergedPublish(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	checkpointPath := harvest.DailyDialogCheckpointPath(filepath.Join(root, "state"))
	oldCheckpoint := harvest.NewDailyDialogCheckpoint(42, "scope", "2026-07-28", []harvest.DailyDialogHead{
		{ChatID: 1, ChatType: "user", TopMessageID: 10, VerifiedMessageID: 10, HeadFullyVerified: true},
	}, time.Unix(1, 0))
	if err := harvest.SaveDailyDialogCheckpoint(checkpointPath, oldCheckpoint); err != nil {
		t.Fatal(err)
	}
	nextCheckpoint := harvest.NewDailyDialogCheckpoint(42, "scope", "2026-07-29", []harvest.DailyDialogHead{
		{ChatID: 1, ChatType: "user", TopMessageID: 11, VerifiedMessageID: 11, HeadFullyVerified: true},
	}, time.Unix(2, 0))

	if _, err := publishDailyCatchupCompletion(reportDir, []string{"2026-07-29"}, checkpointPath, &nextCheckpoint); err == nil {
		t.Fatal("missing daily report should fail merged publication")
	}
	unchanged, err := harvest.LoadDailyDialogCheckpoint(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.VerifiedThrough != oldCheckpoint.VerifiedThrough || unchanged.Dialogs[0].TopMessageID != 10 {
		t.Fatalf("checkpoint advanced before merged publish: %+v", unchanged)
	}

	mustWriteCLIFile(t, filepath.Join(reportDir, "2026-07-29.md"), "# Daily\n\nnew message\n")
	mergedPath, err := publishDailyCatchupCompletion(reportDir, []string{"2026-07-29"}, checkpointPath, &nextCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readCLIFile(t, mergedPath), "new message") {
		t.Fatalf("merged report missing content: %s", mergedPath)
	}
	advanced, err := harvest.LoadDailyDialogCheckpoint(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.VerifiedThrough != "2026-07-29" || advanced.Dialogs[0].TopMessageID != 11 {
		t.Fatalf("checkpoint was not advanced after merged publish: %+v", advanced)
	}
}

func TestExplicitCatchupFromForcesFullCheckpointFallback(t *testing.T) {
	scope := "scope"
	request := dailyCheckpointRequest{
		Previous: harvest.NewDailyDialogCheckpoint(42, scope, "2026-07-28", []harvest.DailyDialogHead{
			{ChatID: 1, ChatType: "user", TopMessageID: 10, VerifiedMessageID: 10, HeadFullyVerified: true},
		}, time.Now()),
		ScopeFingerprint: scope,
		StartDate:        "2026-07-29",
		VerifiedThrough:  "2026-07-29",
		ForceFull:        true,
	}
	decision := evaluateDailyCheckpointRequest(request, 42)
	if decision.Enabled || decision.FallbackReason != "explicit_from" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestRunDailyRangeJobsDoesNotTruncateASRLogsBeforeFirstEvent(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	jobs := []dailyJob{
		testDailyJob(root, "2026-06-03", start),
		testDailyJob(root, "2026-06-04", start.AddDate(0, 0, 1)),
	}
	for _, job := range jobs {
		mustWriteCLIFile(t, job.ASRLogPath, "previous-run\n")
	}
	dumper := &fakeDailyRangeDumper{err: errors.New("telegram unavailable")}

	_, err := runDailyRangeJobs(context.Background(), dumper, harvest.HistoryOptions{}, dailyOptions{}, jobs, nil, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "telegram unavailable") {
		t.Fatalf("error = %v", err)
	}
	for _, job := range jobs {
		if got := readCLIFile(t, job.ASRLogPath); got != "previous-run\n" {
			t.Fatalf("%s ASR log was truncated before an event: %q", job.Date, got)
		}
	}
}

func TestRunDailyRangeJobsDetectsDiscardedASREncodeError(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	job := testDailyJob(root, "2026-06-03", start)
	blockedParent := filepath.Join(root, "blocked")
	mustWriteCLIFile(t, blockedParent, "not-a-directory\n")
	job.ASRLogPath = filepath.Join(blockedParent, "2026-06-03.jsonl")
	dumper := &fakeDailyRangeDumper{
		discardASRErr: true,
		records: []harvest.MessageRecord{
			{Chat: harvest.Chat{ID: 1}, MessageID: 1, Date: start.Add(time.Hour), Outgoing: true, Kind: "voice"},
		},
		stats: harvest.OutgoingStats{Complete: true},
	}

	_, err := runDailyRangeJobs(context.Background(), dumper, harvest.HistoryOptions{}, dailyOptions{}, []dailyJob{job}, nil, &strings.Builder{})
	if err == nil {
		t.Fatal("discarded production-style ASR callback error was not detected")
	}
	if _, statErr := os.Stat(job.OutputPath); !os.IsNotExist(statErr) {
		t.Fatalf("report JSONL should not be published after ASR error: %v", statErr)
	}
	if _, statErr := os.Stat(job.MarkdownPath); !os.IsNotExist(statErr) {
		t.Fatalf("Markdown should not be published after ASR error: %v", statErr)
	}
}

func TestDailyJobAtUsesHalfOpenRangesAndGaps(t *testing.T) {
	start := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	jobs := sortedDailyJobs([]dailyJob{
		{Date: "2026-06-05", Start: start.AddDate(0, 0, 2), End: start.AddDate(0, 0, 3)},
		{Date: "2026-06-03", Start: start, End: start.AddDate(0, 0, 1)},
	})
	if job, ok := dailyJobAt(jobs, start); !ok || job.Date != "2026-06-03" {
		t.Fatalf("start boundary = %+v ok=%t", job, ok)
	}
	if _, ok := dailyJobAt(jobs, start.AddDate(0, 0, 1)); ok {
		t.Fatal("exclusive end/gap boundary should not match")
	}
	if job, ok := dailyJobAt(jobs, start.AddDate(0, 0, 3).Add(-time.Nanosecond)); !ok || job.Date != "2026-06-05" {
		t.Fatalf("last instant = %+v ok=%t", job, ok)
	}
	if _, ok := dailyJobAt(jobs, start.AddDate(0, 0, 3)); ok {
		t.Fatal("range end should be exclusive")
	}
}

func TestAtomicOutputPublishReplacesFinalOnlyOnPublish(t *testing.T) {
	finalPath := filepath.Join(t.TempDir(), "daily.jsonl")
	mustWriteCLIFile(t, finalPath, "old\n")

	tempPath, file, err := createAtomicOutput(finalPath)
	if err != nil {
		t.Fatalf("create atomic output: %v", err)
	}
	if _, err := file.WriteString("new\n"); err != nil {
		t.Fatalf("write temp output: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp output: %v", err)
	}
	if got := readCLIFile(t, finalPath); got != "old\n" {
		t.Fatalf("final changed before publish: %q", got)
	}
	if err := publishAtomicOutput(tempPath, finalPath); err != nil {
		t.Fatalf("publish atomic output: %v", err)
	}
	if got := readCLIFile(t, finalPath); got != "new\n" {
		t.Fatalf("final after publish = %q", got)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("temp file should be renamed away, stat err=%v", err)
	}
}

type fakeDailyRangeDumper struct {
	calls         int
	opts          harvest.OutgoingRangeOptions
	records       []harvest.MessageRecord
	stats         harvest.OutgoingStats
	err           error
	discardASRErr bool
}

func (f *fakeDailyRangeDumper) DumpOutgoingRange(_ context.Context, opts harvest.OutgoingRangeOptions, emit func(harvest.MessageRecord) error) (harvest.OutgoingStats, error) {
	f.calls++
	f.opts = opts
	if f.err != nil {
		return harvest.OutgoingStats{}, f.err
	}
	stats := f.stats
	for _, record := range f.records {
		if opts.IncludeRecord != nil && !opts.IncludeRecord(record) {
			continue
		}
		if opts.History.ASRLog != nil {
			if err := opts.History.ASRLog(harvest.ASRLogEvent{
				At:        record.Date,
				Action:    "cache_hit",
				Date:      record.Date,
				Chat:      record.Chat,
				MessageID: record.MessageID,
			}); err != nil && !f.discardASRErr {
				return harvest.OutgoingStats{}, err
			}
		}
		if err := emit(record); err != nil {
			return harvest.OutgoingStats{}, err
		}
		stats.Records++
	}
	return stats, nil
}

func testDailyJob(root string, date string, start time.Time) dailyJob {
	return dailyJob{
		Date:         date,
		Start:        start,
		End:          start.AddDate(0, 0, 1),
		OutputPath:   filepath.Join(root, "jsonl", date+".jsonl"),
		MarkdownPath: filepath.Join(root, "reports", date+".md"),
		ASRLogPath:   filepath.Join(root, "asr", date+".jsonl"),
	}
}

func readMessageRecords(t *testing.T, path string) []harvest.MessageRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var records []harvest.MessageRecord
	for decoder.More() {
		var record harvest.MessageRecord
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func readASREvents(t *testing.T, path string) []harvest.ASRLogEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var events []harvest.ASRLogEvent
	for decoder.More() {
		var event harvest.ASRLogEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func configForCatchup(root string) config.Config {
	return config.Config{
		StateDir: filepath.Join(root, ".state", "daily"),
	}
}

func dailyJobDates(jobs []dailyJob) []string {
	dates := make([]string, 0, len(jobs))
	for _, job := range jobs {
		dates = append(dates, job.Date)
	}
	return dates
}

func runCommand(t *testing.T, args []string, env map[string]string) (int, string, string) {
	t.Helper()
	baseDir := t.TempDir()
	clearCommandEnv(t)
	t.Setenv("TG_HARVEST_STUDY_APP_ID", "0")
	t.Setenv("TG_HARVEST_STUDY_APP_HASH", "test-hash")
	t.Setenv("TG_HARVEST_STUDY_STATE_DIR", filepath.Join(baseDir, "state"))
	t.Setenv("TG_HARVEST_STUDY_SESSION_PATH", filepath.Join(baseDir, "sessions", "study.json"))
	for key, value := range env {
		t.Setenv(key, value)
	}

	stdout := tempFile(t, "stdout")
	defer os.Remove(stdout.Name())
	stderr := tempFile(t, "stderr")
	defer os.Remove(stderr.Name())
	stdin := tempFile(t, "stdin")
	defer os.Remove(stdin.Name())

	code := run(args, stdin, stdout, stderr)
	return code, readTempFile(t, stdout), readTempFile(t, stderr)
}

func clearCommandEnv(t *testing.T) {
	t.Helper()
	prefixes := []string{
		"TG_HARVEST_DAILY_",
		"TG_HARVEST_STUDY_",
	}
	suffixes := []string{
		"APP_ID",
		"APP_HASH",
		"PHONE",
		"PASSWORD",
		"SESSION_PATH",
		"STATE_DIR",
		"ALLOWED_CHATS",
		"ADDITIONAL_SENDERS",
		"TRANSCRIBE_CMD",
		"VOSK_COMMAND",
		"VOSK_MODEL_PATH",
		"VOSK_GRAMMAR_PATH",
		"VOSK_LIBRARY_PATH",
		"FFMPEG_COMMAND",
		"WHISPER_COMMAND",
		"WHISPER_MODEL_PATH",
		"WHISPER_SPEECH_GATE_MODEL_PATH",
	}
	for _, prefix := range prefixes {
		for _, suffix := range suffixes {
			t.Setenv(prefix+suffix, "")
		}
	}
}

func tempFile(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), name)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	return file
}

func readTempFile(t *testing.T, file *os.File) string {
	t.Helper()
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatalf("seek %s: %v", file.Name(), err)
	}
	content, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("read %s: %v", file.Name(), err)
	}
	return string(content)
}

func mustWriteCLIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readCLIFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
