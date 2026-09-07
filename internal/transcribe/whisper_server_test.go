package transcribe

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWhisperServerRunnerShortFormUsesAutomaticLanguage(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "english.wav")
	if err := os.WriteFile(wavPath, make([]byte, 32044), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer request.MultipartForm.RemoveAll()
		if request.FormValue("language") != "auto" {
			http.Error(w, "short-form language must be auto", http.StatusBadRequest)
			return
		}
		if request.FormValue("detect_language") != "" || request.FormValue("prompt") != "" ||
			request.FormValue("no_timestamps") != "true" || request.FormValue("token_timestamps") != "false" {
			http.Error(w, "unexpected short-form decode options", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(whisperResponse{
			Text:     " an English short-form transcript ",
			Language: "english",
			Segments: []whisperSegment{{AverageLogProbability: -0.1}},
		})
	}))
	defer server.Close()

	opts := ProductionOptions("whisper-server", ProductionModelFile, ProductionSpeechGateFile, "ffmpeg", nil)
	runner := &WhisperServerRunner{opts: opts, baseURL: server.URL, client: server.Client()}
	text, diagnostics, err := runner.inferLocked(t.Context(), wavPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) != "an English short-form transcript" {
		t.Fatalf("text = %q", text)
	}
	if diagnostics == nil || diagnostics.Segments != 1 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestWhisperServerRunnerKeepsModelSession(t *testing.T) {
	if os.Getenv("GO_WANT_WHISPER_HELPER") == "1" {
		runWhisperServerHelper()
		return
	}
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-whisper-server")
	script := fmt.Sprintf(`#!/bin/sh
port=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--port" ]; then port="$2"; shift 2; continue; fi
  shift
done
printf 'ggml_metal_init: found device 0\n' >&2
printf 'WHISPER : COREML = 0\n' >&2
GO_WANT_WHISPER_HELPER=1 exec %q -test.run=TestWhisperServerRunnerKeepsModelSession -- "$port"
`, os.Args[0])
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ffmpeg := filepath.Join(dir, "fake-ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\ncp \"$6\" \"${13}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.bin")
	if err := os.WriteFile(input, make([]byte, 32044), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &WhisperServerRunner{opts: Options{
		WhisperCommand:   command,
		WhisperModelPath: filepath.Join(dir, "model.bin"),
		WhisperThreads:   4,
		Language:         "ru",
		FFmpegCommand:    ffmpeg,
	}}
	defer runner.Close()

	first, err := runner.RunDetailed(t.Context(), input, filepath.Join(dir, "first.txt"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := runner.RunDetailed(t.Context(), input, filepath.Join(dir, "second.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != "привет мир" || second.Text != first.Text {
		t.Fatalf("texts = %q, %q", first.Text, second.Text)
	}
	if first.ModelColdStartDuration <= 0 {
		t.Fatalf("first cold start = %s", first.ModelColdStartDuration)
	}
	if second.ModelColdStartDuration != 0 {
		t.Fatalf("second cold start = %s, want zero", second.ModelColdStartDuration)
	}
	if first.Backend.Accelerator != AcceleratorMetal || first.Backend.Backend != BackendWhisperCPP {
		t.Fatalf("backend = %+v", first.Backend)
	}
	if first.Diagnostics == nil || first.Diagnostics.Segments != 1 ||
		first.Diagnostics.MeanAverageLogProb != -0.25 ||
		first.Diagnostics.MaximumNoSpeechProb != 0.1 {
		t.Fatalf("diagnostics = %+v", first.Diagnostics)
	}
	if runner.ProcessID() <= 0 {
		t.Fatal("whisper process is not running")
	}
}

func TestWhisperSpeechGateParsesPresenceWithoutTrimmingAudio(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-vad")
	script := `#!/bin/sh
if [ "$FAKE_GATE_RESULT" = "speech" ]; then
  printf 'Detected 2 speech segments:\n'
  printf 'Speech segment 0: start = 10.00, end = 20.00\n'
  printf 'Speech segment 1: start = 30.00, end = 40.00\n'
else
  printf 'Detected 0 speech segments:\n'
fi
`
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		WhisperCommand: "server",
		WhisperSpeechGate: WhisperSpeechGateOptions{
			Enabled:   true,
			Command:   command,
			ModelPath: "silero.bin",
		},
		Environment: map[string]string{"FAKE_GATE_RESULT": "speech"},
	}
	segments, err := runWhisperSpeechGate(t.Context(), opts, "input.wav")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatal("speech gate rejected speech")
	}
	opts.Environment["FAKE_GATE_RESULT"] = "silence"
	segments, err = runWhisperSpeechGate(t.Context(), opts, "input.wav")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatal("speech gate accepted no-speech input")
	}
}

func TestWhisperSpeechGateAppliesMinSpeechAfterMinSilence(t *testing.T) {
	minSpeech := 250
	minSilence := 100
	opts := Options{
		WhisperSpeechGate: WhisperSpeechGateOptions{
			Enabled:              true,
			ModelPath:            "silero.bin",
			MinSpeechDurationMS:  &minSpeech,
			MinSilenceDurationMS: &minSilence,
		},
	}
	args := whisperSpeechGateArgs(opts, opts.WhisperSpeechGate.normalized(), "input.wav")
	index := func(flag string) int {
		for current, value := range args {
			if value == flag {
				return current
			}
		}
		return -1
	}
	minSilenceIndex := index("--vad-min-silence-duration-ms")
	minSpeechIndex := index("--vad-min-speech-duration-ms")
	if minSilenceIndex < 0 || minSpeechIndex < 0 || minSilenceIndex >= minSpeechIndex {
		t.Fatalf("gate args do not preserve v1.9.1 min-speech workaround: %v", args)
	}
}

func TestParseVADSpeechSegments(t *testing.T) {
	segments, err := parseVADSpeechSegments("Detected 2 speech segments:\nSpeech segment 0: start = 17949.00, end = 18285.00\nSpeech segment 1: start = 29306.00, end = 29344.00\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].start != 179.49 || segments[0].end != 182.85 || segments[1].end != 293.44 {
		t.Fatalf("segments = %#v", segments)
	}
	segments, err = parseVADSpeechSegments("Detected 0 speech segments:\n")
	if err != nil || len(segments) != 0 {
		t.Fatalf("silence result: segments = %#v, err = %v", segments, err)
	}
	if _, err := parseVADSpeechSegments("unexpected output"); err == nil {
		t.Fatal("malformed VAD output was accepted")
	}
	if _, err := parseVADSpeechSegments("Detected 2 speech segments:\nSpeech segment 0: start = 10.00, end = 20.00\n"); err == nil {
		t.Fatal("incomplete VAD output was accepted")
	}
}

func TestWhisperServerRunnerLongFormDetectsLanguageThenUsesOneTimestampedRequest(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "long.wav")
	if err := os.WriteFile(wavPath, make([]byte, 44+240*32000), 0o600); err != nil {
		t.Fatal(err)
	}
	ffmpegPath := filepath.Join(dir, "fake-ffmpeg")
	ffmpegScript := `#!/bin/sh
last=""
for arg in "$@"; do last="$arg"; done
test "$8" = "15.000" || exit 42
head -c 480044 /dev/zero > "$last"
`
	if err := os.WriteFile(ffmpegPath, []byte(ffmpegScript), 0o700); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := http.Server{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer request.MultipartForm.RemoveAll()
		file, header, err := request.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = file.Close()
		if request.FormValue("detect_language") == "true" {
			if request.FormValue("language") != "auto" || request.FormValue("duration") != "" || request.FormValue("no_timestamps") != "true" {
				http.Error(w, "language probe", http.StatusBadRequest)
				return
			}
			if header.Size != 44+15*32000 {
				http.Error(w, fmt.Sprintf("language probe bytes = %d", header.Size), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(whisperResponse{Language: "russian", Duration: 240})
			return
		}
		if header.Size != 44+240*32000 {
			http.Error(w, fmt.Sprintf("long-form bytes = %d", header.Size), http.StatusBadRequest)
			return
		}
		if request.FormValue("language") != "ru" || request.FormValue("prompt") != longFormRussianInitialPrompt ||
			request.FormValue("carry_initial_prompt") != "false" || request.FormValue("no_timestamps") != "false" ||
			request.FormValue("token_timestamps") != "false" {
			http.Error(w, "timestamp mode", http.StatusBadRequest)
			return
		}
		start0, end0 := 0.05, 119.8
		start1, end1 := 119.8, 120.2
		start2, end2 := 120.2, 239.7
		_ = json.NewEncoder(w).Encode(whisperResponse{
			Text:     " Игорь объясняет Kubernetes прямо через прежнюю границу без разрыва контекста\n" + strings.Repeat("Продолжение следует!\n", 29),
			Duration: 240,
			Segments: []whisperSegment{
				{Start: &start0, End: &end0, AverageLogProbability: -0.2},
				{Start: &start1, End: &end1, AverageLogProbability: -0.1},
				{Start: &start2, End: &end2, AverageLogProbability: -0.1},
			},
		})
	})
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	runner := &WhisperServerRunner{
		opts: Options{Language: "auto", FFmpegCommand: ffmpegPath, WhisperAdaptive: WhisperAdaptiveOptions{
			Enabled:              true,
			LanguageProbeSeconds: longFormLanguageProbeSeconds,
			RussianInitialPrompt: longFormRussianInitialPrompt,
		}},
		baseURL: "http://" + listener.Addr().String(),
		client:  &http.Client{},
	}
	text, diagnostics, err := runner.inferLongFormLocked(t.Context(), wavPath, 239.5)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want one language probe and one native long-form request", requests)
	}
	if strings.TrimSpace(text) != "Игорь объясняет Kubernetes прямо через прежнюю границу без разрыва контекста\nПродолжение следует!" {
		t.Fatalf("text = %q", text)
	}
	if diagnostics == nil || !diagnostics.TimestampedSegments || diagnostics.Segments != 3 ||
		diagnostics.DecodedAudioDurationSeconds != 240 || diagnostics.FirstSegmentStartSeconds != 0.05 ||
		diagnostics.LastSegmentEndSeconds != 239.7 || diagnostics.LastDetectedSpeechEndSeconds != 239.5 ||
		!diagnostics.CoverageValidated || diagnostics.TrailingSpeechCoverageGapSeconds != 0 ||
		diagnostics.DetectedLanguage != "russian" || diagnostics.LanguageDetectionSeconds <= 0 || !diagnostics.InitialPromptApplied ||
		diagnostics.TrailingCoverageToleranceSeconds != longFormCoverageToleranceSeconds || !diagnostics.RepetitionValidated ||
		diagnostics.ExtremeRepetitionDetected || len(diagnostics.RemovedTerminalHallucinations) != 28 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".asr-language-probe-*.wav")); err != nil || len(matches) != 0 {
		t.Fatalf("language probe cleanup: matches=%v err=%v", matches, err)
	}
}

func TestLongFormDecodeRequestAppliesPromptOnlyToRussian(t *testing.T) {
	settings := WhisperAdaptiveOptions{Enabled: true, RussianInitialPrompt: longFormRussianInitialPrompt}.normalized()
	russian, err := longFormDecodeRequest(settings, "Russian")
	if err != nil {
		t.Fatal(err)
	}
	if russian.Language != "ru" || russian.InitialPrompt != longFormRussianInitialPrompt || russian.CarryInitialPrompt || !russian.SegmentTimestamps {
		t.Fatalf("russian request = %#v", russian)
	}
	english, err := longFormDecodeRequest(settings, "English")
	if err != nil {
		t.Fatal(err)
	}
	if english.Language != "en" || english.InitialPrompt != "" || !english.SegmentTimestamps {
		t.Fatalf("english request = %#v", english)
	}
	other, err := longFormDecodeRequest(settings, "Spanish")
	if err != nil {
		t.Fatal(err)
	}
	if other.Language != "auto" || other.InitialPrompt != "" {
		t.Fatalf("other request = %#v", other)
	}
	if _, err := longFormDecodeRequest(settings, ""); err == nil {
		t.Fatal("empty detected language was accepted")
	}
}

func TestValidateTimestampedLongFormResponseRejectsIncompleteMetadata(t *testing.T) {
	start0, end0 := 0.1, 0.8
	start1, end1 := 0.7, 0.6
	tests := []struct {
		name     string
		response whisperResponse
	}{
		{name: "duration", response: whisperResponse{Text: "речь", Duration: 4, Segments: []whisperSegment{{Start: &start0, End: &end0}}}},
		{name: "missing timestamps", response: whisperResponse{Text: "речь", Duration: 1, Segments: []whisperSegment{{}}}},
		{name: "invalid order", response: whisperResponse{Text: "речь", Duration: 1, Segments: []whisperSegment{{Start: &start1, End: &end1}}}},
		{name: "empty transcript", response: whisperResponse{Duration: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateTimestampedLongFormResponse(test.response, 1, 0.8, 0.2); err == nil {
				t.Fatal("invalid long-form response was accepted")
			}
		})
	}
}

func TestValidateTimestampedLongFormResponseRequiresTrailingSpeechCoverage(t *testing.T) {
	start, end := 0.1, 6.5
	decoded := whisperResponse{
		Text:     "речь",
		Duration: 10,
		Segments: []whisperSegment{{Start: &start, End: &end}},
	}
	if gap, err := validateTimestampedLongFormResponse(decoded, 10, 9, 2); err == nil || gap != 2.5 {
		t.Fatalf("uncovered tail: gap=%.3f err=%v", gap, err)
	}
	end = 7.2
	if gap, err := validateTimestampedLongFormResponse(decoded, 10, 9, 2); err != nil || math.Abs(gap-1.8) > 0.001 {
		t.Fatalf("covered tail: gap=%.3f err=%v", gap, err)
	}
}

func TestValidateTimestampedLongFormResponseBoundsOnlyTerminalWindowOverrun(t *testing.T) {
	start0, end0 := 2100.0, 2170.38
	start1, end1 := 2170.38, 2200.36
	decoded := whisperResponse{
		Text:     "финальная реплика",
		Duration: 2171.499,
		Segments: []whisperSegment{
			{Start: &start0, End: &end0},
			{Start: &start1, End: &end1},
		},
	}
	gap, err := validateTimestampedLongFormResponse(decoded, 2171.499, 2171.2, 2)
	if err != nil || gap != 0 {
		t.Fatalf("terminal window overrun: gap=%.3f err=%v", gap, err)
	}
	if end := boundedTerminalSegmentEnd(decoded); end != decoded.Duration {
		t.Fatalf("bounded terminal end = %.3f, want %.3f", end, decoded.Duration)
	}

	badEnd := 2200.0
	decoded.Segments = []whisperSegment{
		{Start: &start0, End: &badEnd},
		{Start: &start1, End: &end1},
	}
	if _, err := validateTimestampedLongFormResponse(decoded, 2171.499, 2171.2, 2); err == nil {
		t.Fatal("non-terminal timestamp overrun was accepted")
	}

	badStart := decoded.Duration
	decoded.Segments = []whisperSegment{{Start: &badStart, End: &end1}}
	if _, err := validateTimestampedLongFormResponse(decoded, 2171.499, 2171.2, 2); err == nil {
		t.Fatal("terminal segment entirely beyond the audio was accepted")
	}
}

func TestStripWhisperTerminalHallucinationsIsConservative(t *testing.T) {
	text := "Реальное содержание.\nСубтитры сделал DimaTorzok"
	got, removed := stripWhisperTerminalHallucinations(text)
	if got != "Реальное содержание." {
		t.Fatalf("filtered text = %q", got)
	}
	if len(removed) != 1 || removed[0] != "Субтитры сделал DimaTorzok" {
		t.Fatalf("removed = %#v", removed)
	}

	got, removed = stripWhisperTerminalHallucinations("Он сказал: продолжение следует, но шутил.")
	if got != "Он сказал: продолжение следует, но шутил." || len(removed) != 0 {
		t.Fatalf("embedded phrase was removed: text=%q removed=%#v", got, removed)
	}

	got, removed = stripWhisperTerminalHallucinations("Спасибо.")
	if got != "Спасибо." || len(removed) != 0 {
		t.Fatalf("ordinary thanks was removed: text=%q removed=%#v", got, removed)
	}
}

func TestStripWhisperLongFormTerminalRepetitionsKeepsOneClosingPhrase(t *testing.T) {
	text := "Спасибо тебе большое.\nУдачи.\nСпасибо.\nспасибо!\nСПАСИБО"
	got, removed := stripWhisperLongFormTerminalRepetitions(text)
	if got != "Спасибо тебе большое.\nУдачи.\nСпасибо." {
		t.Fatalf("filtered text = %q", got)
	}
	if len(removed) != 2 || removed[0] != "спасибо!" || removed[1] != "СПАСИБО" {
		t.Fatalf("removed = %#v", removed)
	}

	got, removed = stripWhisperLongFormTerminalRepetitions("Да.\nДа.\nПродолжаем.")
	if got != "Да.\nДа.\nПродолжаем." || len(removed) != 0 {
		t.Fatalf("non-terminal repetition changed: text=%q removed=%#v", got, removed)
	}
}

func TestAnalyzeWhisperRepetitionRejectsOnlyExtremeExactCycles(t *testing.T) {
	policy := ProductionWhisperAdaptive().normalized()
	normal := analyzeWhisperRepetition("Да, да, продолжаем. Я тестировал, тестировал, тестировал и закончил.", policy)
	if normal.Extreme {
		t.Fatalf("normal repetition marked extreme: %#v", normal)
	}
	extreme := analyzeWhisperRepetition(strings.Repeat("это выдуманный длинный цикл ", 6), policy)
	if !extreme.Extreme || extreme.BlockTokens != 4 || extreme.Repetitions < 5 || extreme.SpanTokens < 20 {
		t.Fatalf("extreme repetition not detected: %#v", extreme)
	}

	stricter := policy
	stricter.RepetitionMinRepeats = 7
	if got := analyzeWhisperRepetition(strings.Repeat("это выдуманный длинный цикл ", 6), stricter); got.Extreme {
		t.Fatalf("non-default repetition policy was ignored: %#v", got)
	}
	limitedBlock := policy
	limitedBlock.RepetitionMaxBlockTokens = 3
	if got := analyzeWhisperRepetition(strings.Repeat("это выдуманный длинный цикл ", 6), limitedBlock); got.Extreme {
		t.Fatalf("max-block policy was ignored: %#v", got)
	}
}

func TestSelectAdaptiveRouteKeepsShortPathAndProtectsRiskyAudio(t *testing.T) {
	settings := ProductionWhisperAdaptive().normalized()
	tests := []struct {
		name           string
		duration       float64
		firstSpeech    float64
		speechDetected bool
		strategy       string
		reason         string
	}{
		{name: "ordinary voice", duration: 150, firstSpeech: 0.4, speechDetected: true, strategy: whisperShortFormStrategy, reason: adaptiveRouteShortMedia},
		{name: "leading silence boundary", duration: 100, firstSpeech: 10, speechDetected: true, strategy: whisperLongFormStrategy, reason: adaptiveRouteLeadingSilence},
		{name: "long boundary", duration: 180, firstSpeech: 0.2, speechDetected: true, strategy: whisperLongFormStrategy, reason: adaptiveRouteDuration},
		{name: "no speech", duration: 900, speechDetected: false, strategy: adaptiveStrategyNoSpeech, reason: adaptiveRouteNoSpeech},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			strategy, reason := selectAdaptiveRoute(settings, test.duration, test.firstSpeech, test.speechDetected)
			if strategy != test.strategy || reason != test.reason {
				t.Fatalf("route = %q/%q, want %q/%q", strategy, reason, test.strategy, test.reason)
			}
		})
	}
}

func runWhisperServerHelper() {
	args := os.Args
	portRaw := args[len(args)-1]
	port, err := strconv.Atoi(portRaw)
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/inference", func(w http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(request.FormValue("language")) != "ru" {
			http.Error(w, "language", http.StatusBadRequest)
			return
		}
		expected := map[string]string{
			"response_format":  "verbose_json",
			"temperature":      "0",
			"temperature_inc":  "0.2",
			"best_of":          "2",
			"beam_size":        "-1",
			"no_speech_thold":  "0.6",
			"logprob_thold":    "-1",
			"entropy_thold":    "2.4",
			"suppress_nst":     "false",
			"no_timestamps":    "true",
			"token_timestamps": "false",
		}
		for key, value := range expected {
			if request.FormValue(key) != value {
				http.Error(w, key, http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(whisperResponse{
			Text: " привет мир ",
			Segments: []whisperSegment{{
				AverageLogProbability: -0.25,
				NoSpeechProbability:   0.1,
			}},
		})
	})
	server := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", port),
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		os.Exit(1)
	}
}
